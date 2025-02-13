package logging

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"
	"github.com/tetratelabs/wazero/internal/wasi_snapshot_preview1"
)

// NewLoggingListenerFactory is an experimental.FunctionListenerFactory that
// logs all functions that have a name to the writer.
//
// Use NewHostLoggingListenerFactory if only interested in host interactions.
func NewLoggingListenerFactory(writer io.Writer) experimental.FunctionListenerFactory {
	return &loggingListenerFactory{writer: writer}
}

// NewHostLoggingListenerFactory is an experimental.FunctionListenerFactory
// that logs exported and host functions to the writer.
//
// This is an alternative to NewLoggingListenerFactory, and would weed out
// guest defined functions such as those implementing garbage collection.
//
// For example, "_start" is defined by the guest, but exported, so would be
// written to the writer in order to provide minimal context needed to
// understand host calls such as "fd_open".
func NewHostLoggingListenerFactory(writer io.Writer) experimental.FunctionListenerFactory {
	return &loggingListenerFactory{writer: writer, hostOnly: true}
}

type loggingListenerFactory struct {
	writer   io.Writer
	hostOnly bool
}

// NewListener implements the same method as documented on
// experimental.FunctionListener.
func (f *loggingListenerFactory) NewListener(fnd api.FunctionDefinition) experimental.FunctionListener {
	exported := len(fnd.ExportNames()) > 0
	if f.hostOnly && // choose functions defined or callable by the host
		fnd.GoFunction() == nil && // not defined by the host
		!exported { // not callable by the host
		return nil
	}

	// special-case formatting of WASI error number until there's a generic way
	// to stringify parameters or results.
	wasiErrnoPos := -1
	if fnd.ModuleName() == "wasi_snapshot_preview1" {
		for i, n := range fnd.ResultNames() {
			if n == "errno" {
				wasiErrnoPos = i
			}
		}
	}
	return &loggingListener{writer: f.writer, fnd: fnd, wasiErrnoPos: wasiErrnoPos}
}

// nestLevelKey holds state between logger.Before and loggingListener.After to ensure
// call depth is reflected.
type nestLevelKey struct{}

// loggingListener implements experimental.FunctionListener to log entrance and exit
// of each function call.
type loggingListener struct {
	writer io.Writer
	fnd    api.FunctionDefinition

	// wasiErrnoPos is the result index of wasi_snapshot_preview1.Errno or -1.
	wasiErrnoPos int
}

// Before logs to stdout the module and function name, prefixed with '-->' and
// indented based on the call nesting level.
func (l *loggingListener) Before(ctx context.Context, _ api.FunctionDefinition, vals []uint64) context.Context {
	nestLevel, _ := ctx.Value(nestLevelKey{}).(int)

	l.writeIndented(true, nil, vals, nestLevel+1)

	// Increase the next nesting level.
	return context.WithValue(ctx, nestLevelKey{}, nestLevel+1)
}

// After logs to stdout the module and function name, prefixed with '<--' and
// indented based on the call nesting level.
func (l *loggingListener) After(ctx context.Context, _ api.FunctionDefinition, err error, vals []uint64) {
	// Note: We use the nest level directly even though it is the "next" nesting level.
	// This works because our indent of zero nesting is one tab.
	l.writeIndented(false, err, vals, ctx.Value(nestLevelKey{}).(int))
}

// writeIndented writes an indented message like this: "-->\t\t\t$indentLevel$funcName\n"
func (l *loggingListener) writeIndented(before bool, err error, vals []uint64, indentLevel int) {
	var message strings.Builder
	for i := 1; i < indentLevel; i++ {
		message.WriteByte('\t')
	}
	if before {
		if l.fnd.GoFunction() != nil {
			message.WriteString("==> ")
		} else {
			message.WriteString("--> ")
		}
		l.writeFuncEnter(&message, vals)
	} else { // after
		if l.fnd.GoFunction() != nil {
			message.WriteString("<==")
		} else {
			message.WriteString("<--")
		}
		l.writeFuncExit(&message, err, vals)
	}
	message.WriteByte('\n')

	_, _ = l.writer.Write([]byte(message.String()))
}

func (l *loggingListener) writeFuncEnter(message *strings.Builder, vals []uint64) {
	valLen := len(vals)
	message.WriteString(l.fnd.DebugName())
	message.WriteByte('(')
	switch valLen {
	case 0:
	default:
		i := l.writeParam(message, 0, vals)
		for i < valLen {
			message.WriteByte(',')
			i = l.writeParam(message, i, vals)
		}
	}
	message.WriteByte(')')
}

func (l *loggingListener) writeFuncExit(message *strings.Builder, err error, vals []uint64) {
	if err != nil {
		message.WriteString(" error: ")
		message.WriteString(err.Error())
		return
	}
	valLen := len(vals)
	if valLen == 0 {
		return
	}
	message.WriteByte(' ')
	switch valLen {
	case 1:
		l.writeResult(message, 0, vals)
	default:
		message.WriteByte('(')
		i := l.writeResult(message, 0, vals)
		for i < valLen {
			message.WriteByte(',')
			i = l.writeResult(message, i, vals)
		}
		message.WriteByte(')')
	}
}

func (l *loggingListener) writeResult(message *strings.Builder, i int, vals []uint64) int {
	if i == l.wasiErrnoPos {
		message.WriteString(wasi_snapshot_preview1.ErrnoName(uint32(vals[i])))
		return i + 1
	}

	if len(l.fnd.ResultNames()) > 0 {
		message.WriteString(l.fnd.ResultNames()[i])
		message.WriteByte('=')
	}

	return l.writeVal(message, l.fnd.ResultTypes()[i], i, vals)
}

func (l *loggingListener) writeParam(message *strings.Builder, i int, vals []uint64) int {
	if len(l.fnd.ParamNames()) > 0 {
		message.WriteString(l.fnd.ParamNames()[i])
		message.WriteByte('=')
	}
	return l.writeVal(message, l.fnd.ParamTypes()[i], i, vals)
}

// writeVal formats integers as signed even though the call site determines
// if it is signed or not. This presents a better experience for values that
// are often signed, such as seek offset. This concedes the rare intentional
// unsigned value at the end of its range will show up as negative.
func (l *loggingListener) writeVal(message *strings.Builder, t api.ValueType, i int, vals []uint64) int {
	v := vals[i]
	i++
	switch t {
	case api.ValueTypeI32:
		message.WriteString(strconv.FormatInt(int64(int32(v)), 10))
	case api.ValueTypeI64:
		message.WriteString(strconv.FormatInt(int64(v), 10))
	case api.ValueTypeF32:
		message.WriteString(strconv.FormatFloat(float64(api.DecodeF32(v)), 'g', -1, 32))
	case api.ValueTypeF64:
		message.WriteString(strconv.FormatFloat(api.DecodeF64(v), 'g', -1, 64))
	case 0x7b: // wasm.ValueTypeV128
		message.WriteString(fmt.Sprintf("%016x%016x", v, vals[i])) // fixed-width hex
		i++
	case api.ValueTypeExternref, 0x70: // wasm.ValueTypeFuncref
		message.WriteString(fmt.Sprintf("%016x", v)) // fixed-width hex
	}
	return i
}
