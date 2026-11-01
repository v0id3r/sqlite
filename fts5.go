// Copyright 2026 The Sqlite Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sqlite // import "modernc.org/sqlite"

import (
	"errors"
	"fmt"
	"sync"
	"unsafe"

	"modernc.org/libc"
	"modernc.org/libc/sys/types"
	sqlite3 "modernc.org/sqlite/lib"
)

// Reasons FTS5 passes to [FTS5Tokenizer.Tokenize] in its flags argument.
// FTS5TokenizePrefix is set together with FTS5TokenizeQuery for a query term
// followed by "*".
const (
	FTS5TokenizeQuery    = int(sqlite3.FTS5_TOKENIZE_QUERY)
	FTS5TokenizePrefix   = int(sqlite3.FTS5_TOKENIZE_PREFIX)
	FTS5TokenizeDocument = int(sqlite3.FTS5_TOKENIZE_DOCUMENT)
	FTS5TokenizeAux      = int(sqlite3.FTS5_TOKENIZE_AUX)
)

// FTS5TokenColocated is the token flag that puts a token at the same position
// as the token emitted before it, the way FTS5 indexes synonyms or stems next
// to the original word. See https://sqlite.org/fts5.html#synonym_support.
const FTS5TokenColocated = int(sqlite3.FTS5_TOKEN_COLOCATED)

// FTS5Tokenizer splits text for an FTS5 table whose tokenize option names it.
//
// Tokenize calls emit once per token, in order. start and end are the byte
// offsets of the token's source text within text; FTS5 uses them for
// highlight() and snippet(). tflags is 0 or [FTS5TokenColocated]. When emit
// returns an error, Tokenize must stop and return that error unchanged: FTS5
// uses it to end tokenization early.
//
// FTS5 serializes calls on one connection, but different connections call
// tokenizers created from the same factory concurrently.
type FTS5Tokenizer interface {
	Tokenize(text string, flags int, emit func(token string, tflags, start, end int) error) error
}

// FTS5TokenizerFactory creates a tokenizer for a table declared with
// tokenize='name arg1 arg2 ...'; args holds the words after the name.
type FTS5TokenizerFactory func(args []string) (FTS5Tokenizer, error)

// RegisterFTS5Tokenizer registers an FTS5 tokenizer on the package-level
// driver, which sql.Open and [NewConnector] use. Like functions, it reaches
// only connections opened after the call.
func RegisterFTS5Tokenizer(name string, factory FTS5TokenizerFactory) error {
	return defaultDriver().RegisterFTS5Tokenizer(name, factory)
}

// RegisterFTS5Tokenizer is like the package-level [RegisterFTS5Tokenizer] but
// registers the tokenizer on d alone. Where d and the package-level driver
// register the same name, the package-level tokenizer wins on d's
// connections, as with [Driver.RegisterModule].
func (d *Driver) RegisterFTS5Tokenizer(name string, factory FTS5TokenizerFactory) error {
	if factory == nil {
		return errors.New("sqlite: nil FTS5 tokenizer factory")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.fts5Tokenizers == nil {
		d.fts5Tokenizers = map[string]FTS5TokenizerFactory{}
	}
	if _, ok := d.fts5Tokenizers[name]; ok {
		return fmt.Errorf("sqlite: FTS5 tokenizer %q already registered", name)
	}
	d.fts5Tokenizers[name] = factory
	return nil
}

type fts5TokenizerKey struct {
	d    *Driver
	name string
}

type goFTS5Tokenizer struct {
	impl FTS5Tokenizer
	// buf holds the token passed to xToken, allocated with the C allocator
	// so the transpiled code can read it.
	buf  uintptr
	size int
}

// fts5TokenError carries a non-OK xToken result back through the Go
// tokenizer, so that xTokenize returns it unchanged as FTS5 requires.
type fts5TokenError struct{ rc int32 }

func (e fts5TokenError) Error() string { return fmt.Sprintf("sqlite: FTS5 xToken returned %d", e.rc) }

var (
	// fts5Factories maps the pUserData of a registered tokenizer to its
	// factory. IDs are stable per registering driver and name.
	fts5Factories = struct {
		mu      sync.RWMutex
		m       map[uintptr]FTS5TokenizerFactory
		ids     idGen
		name2id map[fts5TokenizerKey]uintptr
	}{
		m:       map[uintptr]FTS5TokenizerFactory{},
		name2id: map[fts5TokenizerKey]uintptr{},
	}

	// fts5Tokenizers maps the Fts5Tokenizer handle FTS5 holds to the Go
	// tokenizer instance. The handle is an ID and is never dereferenced.
	fts5Tokenizers = struct {
		mu  sync.RWMutex
		m   map[uintptr]*goFTS5Tokenizer
		ids idGen
	}{
		m: map[uintptr]*goFTS5Tokenizer{},
	}

	// fts5Native holds the C-allocated fts5_tokenizer_v2 every registration
	// passes, and the C string naming the fts5_api pointer type.
	fts5Native struct {
		once      sync.Once
		tokenizer uintptr
		apiType   uintptr
		err       error
	}
)

func fts5NativeInit(tls *libc.TLS) error {
	fts5Native.once.Do(func() {
		p := libc.Xcalloc(tls, 1, types.Size_t(unsafe.Sizeof(sqlite3.Tfts5_tokenizer_v2{})))
		if p == 0 {
			fts5Native.err = errors.New("sqlite: cannot allocate FTS5 tokenizer")
			return
		}
		t := (*sqlite3.Tfts5_tokenizer_v2)(unsafe.Pointer(p))
		t.FiVersion = 2
		t.FxCreate = cFuncPointer(fts5CreateTrampoline)
		t.FxDelete = cFuncPointer(fts5DeleteTrampoline)
		t.FxTokenize = cFuncPointer(fts5TokenizeTrampoline)
		fts5Native.tokenizer = p
		if fts5Native.apiType, fts5Native.err = libc.CString("fts5_api_ptr"); fts5Native.err != nil {
			libc.Xfree(tls, p)
			fts5Native.tokenizer = 0
		}
	})
	return fts5Native.err
}

// registerFTS5Tokenizers installs the tokenizers visible to d on this
// connection, the package-level ones first, mirroring registerModules.
func (c *conn) registerFTS5Tokenizers(d *Driver) error {
	global := defaultDriver()
	if len(global.fts5Tokenizers) == 0 && len(d.fts5Tokenizers) == 0 {
		return nil
	}
	api, err := c.fts5API()
	if err != nil {
		return err
	}
	for name, f := range global.fts5Tokenizers {
		if err := c.registerFTS5Tokenizer(api, global, name, f); err != nil {
			return err
		}
	}
	if d == global {
		return nil
	}
	for name, f := range d.fts5Tokenizers {
		if _, done := global.fts5Tokenizers[name]; done {
			continue
		}
		if err := c.registerFTS5Tokenizer(api, d, name, f); err != nil {
			return err
		}
	}
	return nil
}

// fts5API returns the connection's fts5_api, obtained the documented way: by
// binding a pointer of type "fts5_api_ptr" to SELECT fts5(?1).
func (c *conn) fts5API() (api uintptr, err error) {
	if err := fts5NativeInit(c.tls); err != nil {
		return 0, err
	}
	zSQL, err := libc.CString("SELECT fts5(?1)")
	if err != nil {
		return 0, err
	}
	defer c.free(zSQL)
	pp, err := c.malloc(int(ptrSize))
	if err != nil {
		return 0, err
	}
	defer c.free(pp)
	*(*uintptr)(unsafe.Pointer(pp)) = 0
	psql := zSQL
	pstmt, err := c.prepareV2(&psql)
	if err != nil {
		return 0, err
	}
	defer func() {
		if ferr := c.finalize(pstmt); err == nil {
			err = ferr
		}
	}()
	if rc := sqlite3.Xsqlite3_bind_pointer(c.tls, pstmt, 1, pp, fts5Native.apiType, 0); rc != sqlite3.SQLITE_OK {
		return 0, c.errstr(rc)
	}
	if _, err := c.step(pstmt); err != nil {
		return 0, err
	}
	api = *(*uintptr)(unsafe.Pointer(pp))
	if api == 0 || (*sqlite3.Tfts5_api)(unsafe.Pointer(api)).FiVersion < 3 {
		return 0, errors.New("sqlite: FTS5 tokenizer API v2 is not available")
	}
	return api, nil
}

func (c *conn) registerFTS5Tokenizer(api uintptr, owner *Driver, name string, f FTS5TokenizerFactory) error {
	key := fts5TokenizerKey{owner, name}
	fts5Factories.mu.Lock()
	id, ok := fts5Factories.name2id[key]
	if !ok {
		id = fts5Factories.ids.next()
		fts5Factories.name2id[key] = id
	}
	fts5Factories.m[id] = f
	fts5Factories.mu.Unlock()

	zName, err := libc.CString(name)
	if err != nil {
		return err
	}
	defer c.free(zName)
	create := (*sqlite3.Tfts5_api)(unsafe.Pointer(api)).FxCreateTokenizer_v2
	rc := (*(*func(*libc.TLS, uintptr, uintptr, uintptr, uintptr, uintptr) int32)(unsafe.Pointer(&struct{ uintptr }{create})))(c.tls, api, zName, id, fts5Native.tokenizer, 0)
	if rc != sqlite3.SQLITE_OK {
		return fmt.Errorf("sqlite: register FTS5 tokenizer %q: %w", name, c.errstr(rc))
	}
	return nil
}

func fts5CreateTrampoline(tls *libc.TLS, pUserData uintptr, azArg uintptr, nArg int32, ppOut uintptr) int32 {
	fts5Factories.mu.RLock()
	f := fts5Factories.m[pUserData]
	fts5Factories.mu.RUnlock()
	if f == nil {
		return sqlite3.SQLITE_ERROR
	}
	impl, err := f(extractVtabArgs(tls, nArg, azArg))
	if err != nil || impl == nil {
		return sqlite3.SQLITE_ERROR
	}
	fts5Tokenizers.mu.Lock()
	id := fts5Tokenizers.ids.next()
	fts5Tokenizers.m[id] = &goFTS5Tokenizer{impl: impl}
	fts5Tokenizers.mu.Unlock()
	*(*uintptr)(unsafe.Pointer(ppOut)) = id
	return sqlite3.SQLITE_OK
}

func fts5DeleteTrampoline(tls *libc.TLS, p uintptr) {
	fts5Tokenizers.mu.Lock()
	t := fts5Tokenizers.m[p]
	delete(fts5Tokenizers.m, p)
	if t != nil {
		fts5Tokenizers.ids.reclaim(p)
	}
	fts5Tokenizers.mu.Unlock()
	if t != nil && t.buf != 0 {
		libc.Xfree(tls, t.buf)
	}
}

func fts5TokenizeTrampoline(tls *libc.TLS, p uintptr, pCtx uintptr, flags int32, pText uintptr, nText int32, pLocale uintptr, nLocale int32, xToken uintptr) int32 {
	fts5Tokenizers.mu.RLock()
	t := fts5Tokenizers.m[p]
	fts5Tokenizers.mu.RUnlock()
	if t == nil {
		return sqlite3.SQLITE_ERROR
	}
	var text string
	if nText > 0 {
		text = string(unsafe.Slice((*byte)(unsafe.Pointer(pText)), nText))
	}
	token := (*(*func(*libc.TLS, uintptr, int32, uintptr, int32, int32, int32) int32)(unsafe.Pointer(&struct{ uintptr }{xToken})))
	emit := func(tok string, tflags, start, end int) error {
		if len(tok) > t.size {
			buf := libc.Xrealloc(tls, t.buf, types.Size_t(len(tok)))
			if buf == 0 {
				return fts5TokenError{sqlite3.SQLITE_NOMEM}
			}
			t.buf, t.size = buf, len(tok)
		}
		copy(unsafe.Slice((*byte)(unsafe.Pointer(t.buf)), len(tok)), tok)
		if rc := token(tls, pCtx, int32(tflags), t.buf, int32(len(tok)), int32(start), int32(end)); rc != sqlite3.SQLITE_OK {
			return fts5TokenError{rc}
		}
		return nil
	}
	err := t.impl.Tokenize(text, int(flags), emit)
	var tokErr fts5TokenError
	switch {
	case err == nil:
		return sqlite3.SQLITE_OK
	case errors.As(err, &tokErr):
		return tokErr.rc
	default:
		return sqlite3.SQLITE_ERROR
	}
}
