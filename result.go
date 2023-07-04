package redjet

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Error represents an error returned by the Redis server, as opposed to a
// a protocol or network error.
type Error struct {
	raw string
}

func (e *Error) Error() string {
	return e.raw
}

// IsUnknownCommand returns whether err is an "unknown command" error.
func IsUnknownCommand(err error) bool {
	var e *Error
	return errors.As(err, &e) && strings.HasPrefix(e.raw, "ERR unknown command")
}

// pipeline contains state of a Redis pipeline.
type pipeline struct {
	at  int
	end int
}

// Result is the result of a command.
//
// Its methods are not safe for concurrent use.
type Result struct {
	mu sync.Mutex

	closeCh chan struct{}
	closed  int64

	err error

	conn   *conn
	client *Client

	pipeline pipeline

	// CloseOnRead determines whether the result is closed after the first read.
	// It is set to True when the result is returned from Command, and
	// False when it is returned from Pipeline.
	CloseOnRead bool

	// arrayStack tracks the depth of the array processing. E.g. if we're one
	// level deep with 3 elements remaining, arrayStack will be [3].
	arrayStack []int
}

func (r *Result) Error() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err == nil {
		return ""
	}
	return r.err.Error()
}

func readUntilNewline(rd *bufio.Reader) ([]byte, error) {
	// Simple strings are typically small enough that we can don't care about the allocation.
	b, err := rd.ReadBytes('\n')
	if err != nil {
		return nil, err
	}
	return b[:len(b)-2], nil
}

func readBulkString(c net.Conn, rd *bufio.Reader, w io.Writer) (int, error) {
	c.SetReadDeadline(time.Now().Add(time.Second * 5))
	b, err := rd.ReadBytes('\n')
	if err != nil {
		return 0, err
	}

	n, err := strconv.Atoi(string(b[:len(b)-2]))
	if err != nil {
		return 0, err
	}

	// n == -1 signals a nil value.
	if n == -1 {
		return 0, nil
	}

	// Give about a second per byte.
	c.SetReadDeadline(time.Now().Add(
		time.Second*5 + (time.Duration(n) * time.Second),
	))

	if n == -1 {
		return 0, nil
	}
	var nn int64
	// CopyN should not allocate because rd is a bufio.Reader and implements
	// WriteTo.
	if nn, err = io.CopyN(w, rd, int64(n)); err != nil {
		return int(nn), err
	}
	// Discard CRLF
	if _, err := rd.Discard(2); err != nil {
		return int(nn), err
	}
	return int(nn), nil
}

var errClosed = fmt.Errorf("result closed")

func (r *Result) checkClosed() error {
	if atomic.LoadInt64(&r.closed) != 0 {
		return errClosed
	}
	return nil
}

var _ io.WriterTo = (*Result)(nil)

// WriteTo writes the result to w.
//
// Refer to r.CloseOnRead for whether the result is closed after the first read.
func (r *Result) WriteTo(w io.Writer) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	defer func() {
		if r.CloseOnRead {
			r.close()
		}
	}()

	n, err := r.writeTo(w)
	if err != nil {
		return n, err
	}
	return n, nil
}

// ArrayStack returns the position of the result within an array.
//
// The returned slice must not be modified.
func (r *Result) ArrayStack() []int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.arrayStack
}

// Strings returns the array result as a slice of strings.
func (r *Result) Strings() ([]string, error) {
	// Read the array length.
	n, err := r.Int()
	if err != nil {
		return nil, err
	}

	if len(r.ArrayStack()) == 0 {
		return nil, fmt.Errorf("not in an array")
	}

	// Sanity check that the array length matches the value on the stack.
	if st := r.ArrayStack()[len(r.ArrayStack())-1]; st != n {
		return nil, fmt.Errorf("array stack mismatch (expected %d, got %d)", st, n)
	}

	var ss []string
	for i := 0; i < n; i++ {
		s, err := r.String()
		if err != nil {
			return nil, err
		}
		ss = append(ss, s)
	}
	return ss, nil
}

// writeTo writes the result to w. The second return value is whether or not
// the value indicates an array.
func (r *Result) writeTo(w io.Writer) (int64, error) {
	panic("TODO return the type so that other methods (like array detection) can be more elegantly implemented")

	if err := r.checkClosed(); err != nil {
		return 0, err
	}

	if r.err != nil {
		return 0, r.err
	}

	if r.pipeline.at == r.pipeline.end && len(r.arrayStack) == 0 {
		return 0, fmt.Errorf("no more results")
	}

	r.err = r.conn.wr.Flush()
	if r.err != nil {
		r.err = fmt.Errorf("flush: %w", r.err)
		return 0, r.err
	}

	// The type byte should come fast since its so small. A timeout here implies
	// a protocol error.
	r.conn.SetDeadline(time.Now().Add(time.Second * 5))

	var typ byte
	typ, r.err = r.conn.rd.ReadByte()
	if r.err != nil {
		r.err = fmt.Errorf("read type: %w", r.err)
		return 0, r.err
	}

	// incrRead is how we advance the read state machine.
	incrRead := func() {
		// If we're in an array, we're not making progress on the pipeline,
		// we're just reading the array elements.
		if len(r.arrayStack) > 0 {
			i := len(r.arrayStack) - 1
			r.arrayStack[i]--
			if r.arrayStack[i] == 0 {
				r.arrayStack = r.arrayStack[:i]
			}
		} else {
			r.pipeline.at++
		}
	}

	var s []byte

	switch typ {
	case '+', ':', '*':
		// Simple string or integer
		s, r.err = readUntilNewline(r.conn.rd)
		if r.err != nil {
			return 0, r.err
		}

		isNewArray := typ == '*'
		if isNewArray {
			// New array
			n, err := strconv.Atoi(string(s))
			if err != nil {
				return 0, fmt.Errorf("invalid array length %q", s)
			}
			r.arrayStack = append(r.arrayStack, n)
			return 0, nil
		}

		var n int
		n, r.err = w.Write(s)
		if !isNewArray {
			incrRead()
		}
		return int64(n), r.err
	case '$':
		// Bulk string
		var n int
		n, r.err = readBulkString(r.conn, r.conn.rd, w)
		incrRead()
		return int64(n), r.err
	case '-':
		// Error
		s, r.err = readUntilNewline(r.conn.rd)
		if r.err != nil {
			return 0, r.err
		}
		incrRead()
		return 0, &Error{
			raw: string(s),
		}
	default:
		r.err = fmt.Errorf("unknown type %q", typ)
		return 0, r.err
	}
}

// Bytes returns the result as a byte slice.
//
// Refer to r.CloseOnRead for whether the result is closed after the first read.
func (r *Result) Bytes() ([]byte, error) {
	var buf bytes.Buffer
	_, err := r.WriteTo(&buf)
	return buf.Bytes(), err
}

// String returns the result as a string.
//
// Refer to r.CloseOnRead for whether the result is closed after the first read.
func (r *Result) String() (string, error) {
	var sb strings.Builder
	_, err := r.WriteTo(&sb)
	return sb.String(), err
}

// Int returns the result as an integer.
//
// Refer to r.CloseOnRead for whether the result is closed after the first read.
func (r *Result) Int() (int, error) {
	s, err := r.String()
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(s)
}

// Ok returns whether the result is "OK". Note that it may fail even if the
//
// command succeeded. For example, a successful GET will return a value.
func (r *Result) Ok() error {
	got, err := r.Bytes()
	if err != nil {
		return err
	}
	if !bytes.Equal(got, []byte("OK")) {
		return fmt.Errorf("expected OK, got %q", got)
	}
	return nil
}

// Next returns true if there are more results to read.
func (r *Result) Next() bool {
	if r == nil {
		return false
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	return r.next()
}

// next returns true if there are more results to read.
func (r *Result) next() bool {
	if r.err != nil {
		return false
	}

	return r.pipeline.at < r.pipeline.end || len(r.arrayStack) > 0
}

// Close releases all resources associated with the result.
//
// It is safe to call Close multiple times.
func (r *Result) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.close()
}

func (r *Result) close() error {
	for r.next() {
		// Read the result into discard so that the connection can be reused.
		_, err := r.writeTo(io.Discard)
		if errors.Is(err, errClosed) {
			// Should be impossible to close a result without draining
			// it, in which case at == end and we would never get here.
			return fmt.Errorf("SEVERE: result closed while iterating")
		} else if err != nil {
			return err
		}
	}

	if !atomic.CompareAndSwapInt64(&r.closed, 0, 1) {
		return nil
	}

	if r.closeCh != nil {
		close(r.closeCh)
	}

	if r.err == nil {
		r.client.putConn(r.conn)
	}
	return nil
}

func (r *Result) Array() *ArrayResult {
	return &ArrayResult{
		Result: r,
	}
}

type ArrayResult struct {
	*Result
}
