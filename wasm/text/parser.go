package text

import (
	"errors"
	"fmt"

	"github.com/tetratelabs/wazero/wasm"
)

// currentField holds the positional state of parser. Values are also useful as they allow you to do a reference search
// for all related code including parsers of that position.
type currentField byte

const (
	// fieldInitial is the first position in the source being parsed.
	fieldInitial currentField = iota
	// fieldModule is at the top-level field named "module" and cannot repeat in the same source.
	fieldModule
	// fieldModuleType is at the position module.type and can repeat in the same module.
	//
	// At the start of the field, moduleParser.currentValue0 tracks typeFunc.name. If a field named "func" is
	// encountered, these names are recorded while fieldModuleTypeFunc takes over parsing.
	fieldModuleType
	// fieldModuleTypeFunc is at the position module.type.func and cannot repeat in the same type.
	fieldModuleTypeFunc
	// fieldModuleImport is at the position module.import and can repeat in the same module.
	//
	// At the start of the field, moduleParser.currentValue0 tracks importFunc.module while moduleParser.currentValue1
	// tracks importFunc.name. If a field named "func" is encountered, these names are recorded while
	// fieldModuleImportFunc takes over parsing.
	fieldModuleImport
	// fieldModuleImportFunc is at the position module.import.func and cannot repeat in the same import.
	fieldModuleImportFunc
	// fieldModuleFunc is at the position module.func and can repeat in the same module.
	fieldModuleFunc
	// fieldModuleExport is at the position module.export and can repeat in the same module.
	//
	// At the start of the field, moduleParser.currentValue0 tracks exportFunc.name. If a field named "func" is
	// encountered, these names are recorded while fieldModuleExportFunc takes over parsing.
	fieldModuleExport
	// fieldModuleExportFunc is at the position module.export.func and cannot repeat in the same export.
	fieldModuleExportFunc
	// fieldModuleStart is at the position module.start and cannot repeat in the same module.
	fieldModuleStart
)

type moduleParser struct {
	// tokenParser primarily supports dispatching parse to a different tokenParser depending on the position in the file
	// The initial value is ensureLParen because %.wat files must begin with a '(' token (ignoring whitespace).
	//
	// Design Note: This is an alternative to using a stack as the structure defined in "module" is fixed depth, except
	// function bodies. Any function body may be parsed in a more dynamic way.
	tokenParser tokenParser

	// source is the entire WebAssembly text format source code being parsed.
	source []byte

	// module holds the fields incrementally parsed from tokens in the source.
	module *module

	// currentField is the parser and error context.
	// This is set after reading a field name, ex "module", or after reaching the end of one, ex ')'.
	currentField currentField

	// currentValue0 is a currentField-specific place to stash a string when parsing a field.
	// Ex. for fieldModuleImport, this would be Math if (import "Math" "PI" ...)
	currentValue0 []byte

	// currentValue1 is a currentField-specific place to stash a string when parsing a field.
	// Ex. for fieldModuleImport, this would be PI if (import "Math" "PI" ...)
	currentValue1 []byte

	// currentTypeIndex allows us to track the relative position of module.types regardless of position in the source.
	currentTypeIndex uint32

	// currentFuncIndex allows us to track the relative position of imported or module defined functions.
	currentFuncIndex uint32

	// currentExportIndex allows us to track the relative position of module.exportFuncs regardless of position in the source.
	currentExportIndex uint32

	typeParser  *typeParser
	indexParser *indexParser
	funcParser  *funcParser

	// typeIDContext resolves symbolic identifiers, such as "v_v" to a numeric index in wasm.Module TypeSection, such
	// as '2'. Duplicate identifiers are not allowed by specification.
	//
	// Note: This is not encoded in the wasm.NameSection as there is no type name section in WebAssembly 1.0 (MVP)
	//
	// See https://www.w3.org/TR/wasm-core-1/#text-context
	typeIDContext idContext

	// funcIDContext resolves symbolic identifiers, such as "main" to a numeric index in function index namespace, such
	// as '2'. Duplicate identifiers are not allowed by specification.
	//
	// Note: the function index namespace starts with any wasm.ImportKindFunc in the wasm.Module TypeSection followed by
	// the wasm.Module FunctionSection.
	//
	// Note: this should be updated alongside module.names FunctionNames
	//
	// See https://www.w3.org/TR/wasm-core-1/#text-context
	funcIDContext idContext
}

// parse has the same signature as tokenParser and called by lex on each token.
//
// The tokenParser this dispatches to should be updated when reading a new field name, and restored to the prior
// value or a different parser on endField.
func (p *moduleParser) parse(tok tokenType, tokenBytes []byte, line, col uint32) error {
	return p.tokenParser(tok, tokenBytes, line, col)
}

// parseModule parses the configured source into a module. This function returns when the source is exhausted or an
// error occurs.
//
// Here's a description of the return values:
// * module is the result of parsing or nil on error
// * err is a FormatError invoking the parser, dangling block comments or unexpected characters.
func parseModule(source []byte) (*module, error) {
	p := moduleParser{
		source:        source,
		module:        &module{names: &wasm.NameSection{}},
		indexParser:   &indexParser{},
		typeIDContext: idContext{}, // initialize contexts to reduce the amount of runtime nil checks
		funcIDContext: idContext{},
	}
	p.typeParser = &typeParser{m: &p, paramIDContext: idContext{}}
	p.funcParser = &funcParser{m: &p, onBodyEnd: p.parseFuncEnd}

	// A valid source must begin with the token '(', but it could be preceded by whitespace or comments. For this
	// reason, we cannot enforce source[0] == '(', and instead need to start the lexer to check the first token.
	p.tokenParser = p.ensureLParen
	line, col, err := lex(p.parse, p.source)
	if err != nil {
		return nil, &FormatError{line, col, p.errorContext(), err}
	}

	// Add any types implicitly defined from type use. Ex. (module (import (func (param i32)...
	p.module.types = append(p.module.types, p.typeParser.inlinedTypes...)

	// Ensure indices only point to numeric values
	if err = bindIndices(p.module, p.typeIDContext, p.funcIDContext); err != nil {
		return nil, err
	}

	// Don't set the name section unless we found a name!
	names := p.module.names
	if names.ModuleName == "" && names.FunctionNames == nil && names.LocalNames == nil {
		p.module.names = nil
	}

	return p.module, nil
}

func (p *moduleParser) ensureLParen(tok tokenType, tokenBytes []byte, _, _ uint32) error {
	if tok != tokenLParen {
		return fmt.Errorf("expected '(', but found %s: %s", tok, tokenBytes)
	}
	p.tokenParser = p.beginField
	return nil
}

// beginField assigns the correct moduleParser.currentField and moduleParser.parseModule based on the source position
// and fieldName being read.
//
// Once the next parser reaches a tokenRParen, moduleParser.endField must be called. This means that there must be
// parity between the currentField values handled here and those handled in moduleParser.endField
//
// TODO: this design will likely be revisited to introduce a type that handles both begin and end of the current field.
func (p *moduleParser) beginField(tok tokenType, fieldName []byte, _, _ uint32) error {
	if tok != tokenKeyword {
		return fmt.Errorf("expected field, but found %s", tok)
	}

	// We expect p.currentField set according to a potentially nested "($fieldName".
	// Each case must return a tokenParser that consumes the rest of the field up to the ')'.
	// Note: each branch must handle any nesting concerns. Ex. "(module (import" nests further to "(func".
	p.tokenParser = nil
	switch p.currentField {
	case fieldInitial:
		if string(fieldName) == "module" {
			p.currentField = fieldModule
			p.tokenParser = p.parseModuleName
		}
	case fieldModule:
		switch string(fieldName) {
		case "type":
			p.currentField = fieldModuleType
			p.tokenParser = p.parseTypeID
		case "import":
			p.currentField = fieldModuleImport
			p.tokenParser = p.parseImportModule
		case "func":
			p.currentField = fieldModuleFunc
			p.tokenParser = p.parseFuncID
		case "export":
			p.currentField = fieldModuleExport
			p.tokenParser = p.parseExportName
		case "start":
			if p.module.startFunction != nil {
				return errors.New("redundant start")
			}
			p.currentField = fieldModuleStart
			p.tokenParser = p.indexParser.beginParsingIndex(p.parseStartEnd)
		}
	case fieldModuleType:
		if string(fieldName) == "func" {
			p.currentField = fieldModuleTypeFunc
			p.tokenParser = p.parseTypeFunc
		}
	case fieldModuleImport:
		// Add the next import func object and ready for parsing it.
		if string(fieldName) == "func" {
			p.module.importFuncs = append(p.module.importFuncs, &importFunc{
				module: string(p.currentValue0),
				name:   string(p.currentValue1),
			})

			p.currentField = fieldModuleImportFunc
			p.tokenParser = p.parseImportFuncID
		} // TODO: table, memory or global
	case fieldModuleExport:
		// Add the next export func object and ready for parsing it.
		if string(fieldName) == "func" {
			p.module.exportFuncs = append(p.module.exportFuncs, &exportFunc{
				name:        string(p.currentValue0),
				exportIndex: p.currentExportIndex,
			})

			p.currentField = fieldModuleExportFunc
			p.tokenParser = p.indexParser.beginParsingIndex(p.parseExportFuncEnd)
		} // TODO: table, memory or global
	}
	if p.tokenParser == nil {
		return fmt.Errorf("unexpected field: %s", string(fieldName))
	}
	return nil
}

// endField should be called after encountering tokenRParen. It places the current parser at the parent position based
// on fixed knowledge of the text format structure.
//
// Design Note: This is an alternative to using a stack as the structure parsed by moduleParser is fixed depth. For
// example, any function body may be parsed in a more dynamic way.
func (p *moduleParser) endField() {
	switch p.currentField {
	case fieldModule:
		p.currentField = fieldInitial
		p.tokenParser = p.parseUnexpectedTrailingCharacters // only one module is allowed and nothing else
	case fieldModuleType, fieldModuleImport, fieldModuleFunc, fieldModuleExport, fieldModuleStart:
		p.currentField = fieldModule
		p.tokenParser = p.parseModule
	default: // currentField is an enum, we expect to have handled all cases above. panic if we didn't
		panic(fmt.Errorf("BUG: unhandled parsing state on endField: %v", p.currentField))
	}
}

// parseModuleName is the first parser inside the module field. This records the module.name if present and sets the
// next parser to parseModule. If the token isn't a tokenID, this calls parseModule.
//
// Ex. A module name is present `(module $math)`
//                        records math --^
//
// Ex. No module name `(module)`
//   calls parseModule here --^
func (p *moduleParser) parseModuleName(tok tokenType, tokenBytes []byte, line, col uint32) error {
	if tok == tokenID { // Ex. $Math
		p.module.names.ModuleName = string(stripDollar(tokenBytes))
		p.tokenParser = p.parseModule
		return nil
	}
	return p.parseModule(tok, tokenBytes, line, col)
}

func (p *moduleParser) parseModule(tok tokenType, tokenBytes []byte, _, _ uint32) error {
	switch tok {
	case tokenID:
		return fmt.Errorf("redundant ID %s", tokenBytes)
	case tokenLParen:
		p.tokenParser = p.beginField // after this look for a field name
		return nil
	case tokenRParen: // end of module
		p.endField()
	default:
		return unexpectedToken(tok, tokenBytes)
	}
	return nil
}

// parseTypeID is the first parser inside a type field. This records the symbolic ID, if present, or calls parseType
// if not found.
//
// Ex. A type ID is present `(type $t0 (func (result i32)))`
//                    records t0 --^   ^
//            parseType resumes here --+
//
// Ex. No type ID `(type (func (result i32)))`
//     calls parseType --^
func (p *moduleParser) parseTypeID(tok tokenType, tokenBytes []byte, line, col uint32) error {
	if tok == tokenID { // Ex. $v_v
		p.tokenParser = p.parseType
		return p.setTypeID(tokenBytes)
	}
	return p.parseType(tok, tokenBytes, line, col)
}

// parseType is the last parser inside the type field. This records the func field or errs if missing. When complete,
// this sets the next parser to parseModule.
//
// Ex. func is present `(module (type $rf32 (func (result f32))))`
//                            starts here --^                   ^
//                                   parseModule resumes here --+
//
// Ex. func is missing `(type $rf32 )`
//                      errs here --^
func (p *moduleParser) parseType(tok tokenType, tokenBytes []byte, _, _ uint32) error {
	switch tok {
	case tokenID:
		return fmt.Errorf("redundant ID %s", tokenBytes)
	case tokenLParen: // start fields, ex. (func
		// Err if there's a second func. Ex. (type (func) (func))
		if uint32(len(p.module.types)) > p.currentTypeIndex {
			return unexpectedToken(tok, tokenBytes)
		}
		p.tokenParser = p.beginField
		return nil
	case tokenRParen: // end of this type
		// Err if we never reached a description...
		if uint32(len(p.module.types)) == p.currentTypeIndex {
			return errors.New("missing func field")
		}

		// Multiple types are allowed, so advance in case there's a next.
		p.currentTypeIndex++
		p.endField()
	default:
		return unexpectedToken(tok, tokenBytes)
	}
	return nil
}

// parseTypeFunc is the second parser inside the type field. This passes control to the typeParser until
// any signature is read, then sets the next parser to parseTypeFuncEnd.
//
// Ex. `(module (type $rf32 (func (result f32))))`
//            starts here --^                 ^
//            parseTypeFuncEnd resumes here --+
//
// Ex. If there is no signature `(module (type $rf32 ))`
//                    calls parseTypeFuncEnd here ---^
func (p *moduleParser) parseTypeFunc(tok tokenType, tokenBytes []byte, line, col uint32) error {
	p.typeParser.reset()
	if tok == tokenLParen {
		p.typeParser.beginType(p.parseTypeFuncEnd)
		return nil // start fields, ex. (param or (result
	}
	return p.parseTypeFuncEnd(tok, tokenBytes, line, col) // ended with no parameters
}

// parseTypeFuncEnd is the last parser of the "func" field. As there is no alternative to ending the field, this ensures
// the token is tokenRParen and sets the next parser to parseType on tokenRParen.
func (p *moduleParser) parseTypeFuncEnd(tok tokenType, tokenBytes []byte, _, _ uint32) error {
	if tok == tokenRParen {
		p.module.types = append(p.module.types, p.typeParser.getType())
		p.currentValue0 = nil
		p.currentField = fieldModuleType
		p.tokenParser = p.parseType
		return nil
	}
	return unexpectedToken(tok, tokenBytes)
}

// parseImportModule is the first parser inside the import field. This records the importFunc.module, then sets the next
// parser to parseImportName. Since the imported module name is required, this errs on anything besides tokenString.
//
// Ex. Imported module name is present `(import "Math" "PI" (func (result f32)))`
//                                records Math --^     ^
//                      parseImportName resumes here --+
//
// Ex. Imported module name is absent `(import (func (result f32)))`
//                                 errs here --^
func (p *moduleParser) parseImportModule(tok tokenType, tokenBytes []byte, _, _ uint32) error {
	switch tok {
	case tokenString: // Ex. "" or "Math"
		p.currentValue0 = tokenBytes[1 : len(tokenBytes)-1] // unquote
		p.tokenParser = p.parseImportName
		return nil
	case tokenLParen, tokenRParen:
		return errors.New("missing module and name")
	default:
		return unexpectedToken(tok, tokenBytes)
	}
}

// parseImportName is the second parser inside the import field. This records the importFunc.name, then sets the next
// parser to parseImport. Since the imported function name is required, this errs on anything besides tokenString.
//
// Ex. Imported function name is present `(import "Math" "PI" (func (result f32)))`
//                                         starts here --^    ^
//                                           records PI --^   |
//                                 parseImport resumes here --+
//
// Ex. Imported function name is absent `(import "Math" (func (result f32)))`
//                                          errs here --+
func (p *moduleParser) parseImportName(tok tokenType, tokenBytes []byte, _, _ uint32) error {
	switch tok {
	case tokenString: // Ex. "" or "PI"
		p.currentValue1 = tokenBytes[1 : len(tokenBytes)-1] // unquote
		p.tokenParser = p.parseImport
		return nil
	case tokenLParen, tokenRParen:
		return errors.New("missing name")
	default:
		return unexpectedToken(tok, tokenBytes)
	}
}

// parseImport is the last parser inside the import field. This records the description field, ex. (func) or errs if
// missing. When complete, this sets the next parser to parseModule.
//
// Ex. description is present `(module (import "Math" "PI" (func (result f32))))`
//                                           starts here --^                   ^
//                                                  parseModule resumes here --+
//
// Ex. description is missing `(import "Math" "PI")`
//                                    errs here --^
func (p *moduleParser) parseImport(tok tokenType, tokenBytes []byte, _, _ uint32) error {
	switch tok {
	case tokenString: // Ex. (import "Math" "PI" "PI"
		return fmt.Errorf("redundant name: %s", tokenBytes[1:len(tokenBytes)-1]) // unquote
	case tokenLParen: // start fields, ex. (func
		// Err if there's a second description. Ex. (import "" "" (func) (func))
		if uint32(len(p.module.importFuncs)) > p.currentFuncIndex {
			return unexpectedToken(tok, tokenBytes)
		}
		p.tokenParser = p.beginField
		return nil
	case tokenRParen: // end of this import
		// Err if we never reached a description...
		if uint32(len(p.module.importFuncs)) == p.currentFuncIndex {
			return errors.New("missing description field") // Ex. missing (func): (import "Math" "Pi")
		}

		// Multiple imports are allowed, so advance in case there's a next.
		p.currentFuncIndex++

		// Reset parsing state: this is late to help give correct error messages on multiple descriptions.
		p.currentValue0, p.currentValue1 = nil, nil
		p.endField()
	default:
		return unexpectedToken(tok, tokenBytes)
	}
	return nil
}

// parseImportFuncID is the first parser inside an imported function field. This records the symbolic ID, if present,
// and sets the next parser to parseImportFunc. If the token isn't a tokenID, this calls parseImportFunc.
//
// Ex. A function ID is present `(import "Math" "PI" (func $math.pi (result f32))`
//                                  records math.pi here --^
//                                   parseImportFunc resumes here --^
//
// Ex. No function ID `(import "Math" "PI" (func (result f32))`
//                  calls parseImportFunc here --^
func (p *moduleParser) parseImportFuncID(tok tokenType, tokenBytes []byte, line, col uint32) error {
	if tok == tokenID { // Ex. $main
		p.tokenParser = p.parseImportFunc
		return p.setFuncID(tokenBytes)
	}
	return p.parseImportFunc(tok, tokenBytes, line, col)
}

// setTypeID adds the normalized ('$' stripped) type ID to the typeIDContext.
func (p *moduleParser) setTypeID(idToken []byte) error {
	idx := p.currentTypeIndex
	_, err := p.typeIDContext.setID(idToken, idx)
	return err
}

// setFuncID adds the normalized ('$' stripped) function ID to the funcIDContext and the wasm.NameSection.
func (p *moduleParser) setFuncID(idToken []byte) error {
	idx := p.currentFuncIndex
	id, err := p.funcIDContext.setID(idToken, idx)
	if err != nil {
		return err
	}
	p.module.names.FunctionNames = append(p.module.names.FunctionNames, &wasm.NameAssoc{Index: idx, Name: id})
	return nil
}

// parseImportFunc is the second parser inside the imported function field. This passes control to the typeParser until
// any signature is read, then sets the next parser to parseImportFuncEnd.
//
// Ex. `(import "Math" "PI" (func $math.pi (result f32)))`
//                           starts here --^           ^
//                   parseImportFuncEnd resumes here --+
//
// Ex. If there is no signature `(import "" "main" (func))`
//                     calls parseImportFuncEnd here ---^
func (p *moduleParser) parseImportFunc(tok tokenType, tokenBytes []byte, line, col uint32) error {
	p.typeParser.reset() // reset now in case there is never a tokenLParen
	switch tok {
	case tokenID: // Ex. (func $main $main)
		return fmt.Errorf("redundant ID %s", tokenBytes)
	case tokenLParen:
		p.typeParser.beginTypeUse(p.parseImportFuncEnd, p.parseImportFuncEnd) // start fields, ex. (param or (result
		return nil
	}
	return p.parseImportFuncEnd(tok, tokenBytes, line, col)
}

// parseImportFuncEnd is the last parser inside the imported function field. This records the importFunc.typeIndex
// and/or importFunc.typeInlined and sets the next parser to parseImport.
func (p *moduleParser) parseImportFuncEnd(tok tokenType, tokenBytes []byte, _, _ uint32) error {
	if tok == tokenRParen {
		tu, _, paramNames := p.typeParser.getTypeUse()
		p.module.typeUses = append(p.module.typeUses, tu)
		if paramNames != nil {
			na := &wasm.NameMapAssoc{Index: p.currentFuncIndex, NameMap: paramNames}
			p.module.names.LocalNames = append(p.module.names.LocalNames, na)
		}
		p.currentField = fieldModuleImport
		p.tokenParser = p.parseImport
		return nil
	}
	return unexpectedToken(tok, tokenBytes)
}

// parseFuncID is the first parser inside a function field. This records the function ID, if present, and sets the next
// parser to parseFunc. If the token isn't a tokenID, this calls parseFunc.
//
// Ex. A function ID is present `(module (func $math.pi (result f32))`
//                      records math.pi here --^
//                             parseFunc resumes here --^
//
// Ex. No function ID `(module (func (result f32))`
//            calls parseFunc here --^
func (p *moduleParser) parseFuncID(tok tokenType, tokenBytes []byte, line, col uint32) error {
	if tok == tokenID { // Ex. $main
		p.tokenParser = p.parseFunc
		return p.setFuncID(tokenBytes)
	}
	return p.parseFunc(tok, tokenBytes, line, col)
}

// parseFunc is the second parser inside a module defined function field. This passes control to the typeParser until
// any signature is read, then funcParser for any locals. Finally, this sets the next parser to parseFuncEnd.
//
// Ex. `(module (func $math.pi (result f32))`
//               starts here --^           ^
//             parseFuncEnd resumes here --+
//
// Ex.    `(module (func $math.pi (result f32) (local i32) )`
//                  starts here --^            ^           ^
// funcParser.beginBody resumes here --+           |
//                             parseFuncEnd resumes here --+
//
// Ex. If there is no signature `(func)`
//         calls parseFuncEnd here ---^
func (p *moduleParser) parseFunc(tok tokenType, tokenBytes []byte, line, col uint32) error {
	p.typeParser.reset() // reset now in case there is never a tokenLParen
	switch tok {
	case tokenID: // Ex. (func $main $main)
		return fmt.Errorf("redundant ID %s", tokenBytes)
	case tokenLParen: // start fields, ex. (local or (i32.const
		p.typeParser.beginTypeUse(p.parseFuncBody, p.parseFuncBodyField)
		return nil
	}
	return p.parseFuncEnd(tok, tokenBytes, line, col)
}

func (p *moduleParser) parseFuncBody(tok tokenType, tokenBytes []byte, line, col uint32) error {
	return p.funcParser.beginBody()(tok, tokenBytes, line, col)
}

func (p *moduleParser) parseFuncBodyField(tok tokenType, tokenBytes []byte, line, col uint32) error {
	return p.funcParser.beginBodyField()(tok, tokenBytes, line, col)
}

// parseFuncEnd is the last parser inside the ed function field. This records the Func.typeIndex
// and/or Func.typeInlined and sets the next parser to parse.
func (p *moduleParser) parseFuncEnd(tok tokenType, tokenBytes []byte, _, _ uint32) error {
	if tok == tokenRParen {
		tu, _, paramNames := p.typeParser.getTypeUse()
		p.module.typeUses = append(p.module.typeUses, tu)
		p.module.code = append(p.module.code, &wasm.Code{Body: p.funcParser.getBody()})

		// TODO: locals and also check they don't conflict with paramIDs returned from the type use
		// Note: locals may be unverifiable wrt ID collision if the type isn't known, yet (ex func before type)
		if paramNames != nil {
			na := &wasm.NameMapAssoc{Index: p.currentFuncIndex, NameMap: paramNames}
			p.module.names.LocalNames = append(p.module.names.LocalNames, na)
		}

		// Multiple funcs are allowed, so advance in case there's a next.
		p.currentFuncIndex++
		p.endField()
		return nil
	}
	return unexpectedToken(tok, tokenBytes)
}

// parseExportName is the first parser inside the export field. This records the exportFunc.name, then sets the next
// parser to parseExport. Since the exported function name is required, this errs on anything besides tokenString.
//
// Ex. Exported function name is present `(export "PI" (func 0))`
//                                  starts here --^    ^
//                                records PI --^       |
//                          parseExport resumes here --+
//
// Ex. Exported function name is absent `(export (func 0))`
//                                   errs here --+
func (p *moduleParser) parseExportName(tok tokenType, tokenBytes []byte, _, _ uint32) error {
	switch tok {
	case tokenString: // Ex. "" or "PI"
		p.currentValue0 = tokenBytes[1 : len(tokenBytes)-1] // unquote
		p.tokenParser = p.parseExport

		// verify the name is unique. Note: this logic will be undone on next PR
		if p.module.exportFuncs == nil {
			return nil
		}
		name := string(p.currentValue0)
		for _, e := range p.module.exportFuncs {
			if e.name == name {
				return fmt.Errorf("duplicate name %q", name)
			}
		}
		return nil
	case tokenLParen, tokenRParen:
		return errors.New("missing name")
	default:
		return unexpectedToken(tok, tokenBytes)
	}
}

// parseExport is the last parser inside the export field. This records the description field, ex. (func) or errs if
// missing. When complete, this sets the next parser to parseModule.
//
// Ex. description is present `(export "PI" (func 0))`
//                            starts here --^       ^
//                       parseModule resumes here --+
//
// Ex. description is missing `(export "PI")`
//                             errs here --^
func (p *moduleParser) parseExport(tok tokenType, tokenBytes []byte, _, _ uint32) error {
	switch tok {
	case tokenString: // Ex. (export "PI" "PI"
		return fmt.Errorf("redundant name: %s", tokenBytes[1:len(tokenBytes)-1]) // unquote
	case tokenLParen: // start fields, ex. (func
		// Err if there's a second description. Ex. (export "" "" (func) (func))
		if uint32(len(p.module.exportFuncs)) > p.currentExportIndex {
			return unexpectedToken(tok, tokenBytes)
		}
		p.tokenParser = p.beginField
		return nil
	case tokenRParen: // end of this export
		// Err if we never reached a description...
		if uint32(len(p.module.exportFuncs)) == p.currentExportIndex {
			return errors.New("missing description field") // Ex. missing (func): (export "Math" "Pi")
		}

		// Multiple exports are allowed, so advance in case there's a next.
		p.currentExportIndex++

		// Reset parsing state: this is late to help give correct error messages on multiple descriptions.
		p.currentValue0 = nil
		p.endField()
	default:
		return unexpectedToken(tok, tokenBytes)
	}
	return nil
}

func (p *moduleParser) parseExportFuncEnd(funcidx *index) {
	p.module.exportFuncs[p.currentExportIndex].funcIndex = funcidx
	p.currentField = fieldModuleExport
	p.tokenParser = p.parseExport
}

func (p *moduleParser) parseStartEnd(funcidx *index) {
	p.module.startFunction = funcidx
	p.endField()
}

func (p *moduleParser) parseUnexpectedTrailingCharacters(_ tokenType, tokenBytes []byte, _, _ uint32) error {
	return fmt.Errorf("unexpected trailing characters: %s", tokenBytes)
}

func unexpectedToken(tok tokenType, tokenBytes []byte) error {
	if tok == tokenLParen { // unbalanced tokenRParen is caught at the lexer layer
		return errors.New("unexpected '('")
	}
	return fmt.Errorf("unexpected %s: %s", tok, tokenBytes)
}

func (p *moduleParser) errorContext() string {
	switch p.currentField {
	case fieldInitial:
		return ""
	case fieldModule:
		return "module"
	case fieldModuleType:
		return fmt.Sprintf("module.type[%d]", p.currentTypeIndex)
	case fieldModuleTypeFunc:
		return fmt.Sprintf("module.type[%d].func%s", p.currentTypeIndex, p.typeParser.errorContext())
	case fieldModuleImport:
		return fmt.Sprintf("module.import[%d]", p.currentFuncIndex)
	case fieldModuleImportFunc: // TODO: table, memory or global
		return fmt.Sprintf("module.import[%d].func%s", p.currentFuncIndex, p.typeParser.errorContext())
	case fieldModuleFunc:
		context := p.typeParser.errorContext()
		if context == "" {
			context = p.funcParser.errorContext()
		}
		return fmt.Sprintf("module.func[%d]%s", p.currentFuncIndex, context)
	case fieldModuleExport:
		return fmt.Sprintf("module.export[%d]", p.currentExportIndex)
	case fieldModuleExportFunc: // TODO: table, memory or global
		return fmt.Sprintf("module.export[%d].func%s", p.currentExportIndex, p.typeParser.errorContext())
	case fieldModuleStart:
		return "module.start"
	default: // currentField is an enum, we expect to have handled all cases above. panic if we didn't
		panic(fmt.Errorf("BUG: unhandled parsing state on errorContext: %v", p.currentField))
	}
}
