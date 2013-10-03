// Copyright (c) 2012, Suryandaru Triandana <syndtr@gmail.com>
// All rights reservefs.
//
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package storage

import (
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/syndtr/goleveldb/leveldb/util"
)

var errFileOpen = errors.New("leveldb/storage: file still open")

type fileLock interface {
	release() error
}

type fileStorageLock struct {
	fs *fileStorage
}

func (lock *fileStorageLock) Release() {
	fs := lock.fs
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if fs.slock == lock {
		fs.slock = nil
	}
	return
}

// fileStorage is a file-system backed storage.
type fileStorage struct {
	path string

	mu    sync.Mutex
	flock fileLock
	slock *fileStorageLock
	logw  *os.File
	buf   []byte
	// Opened file counter; if open < 0 means closed.
	open int
}

// OpenFile returns a new filesytem-backed storage implementation with the given
// path. This also hold a file lock, so any subsequent attempt to open the same
// path will fail.
//
// The storage must be closed after use, by calling Close method.
func OpenFile(path string) (Storage, error) {
	if err := os.MkdirAll(path, 0755); err != nil {
		return nil, err
	}

	flock, err := newFileLock(filepath.Join(path, "LOCK"))
	if err != nil {
		return nil, err
	}

	defer func() {
		if err != nil {
			flock.release()
		}
	}()

	rename(filepath.Join(path, "LOG"), filepath.Join(path, "LOG.old"))
	logw, err := os.OpenFile(filepath.Join(path, "LOG"), os.O_WRONLY|os.O_CREATE, 0644)
	if err != nil {
		return nil, err
	}

	fs := &fileStorage{path: path, flock: flock, logw: logw}
	runtime.SetFinalizer(fs, (*fileStorage).Close)
	return fs, nil
}

func (fs *fileStorage) Lock() (util.Releaser, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if fs.open < 0 {
		return nil, ErrClosed
	}
	if fs.slock != nil {
		return nil, ErrLocked
	}
	fs.slock = &fileStorageLock{fs: fs}
	return fs.slock, nil
}

func itoa(buf []byte, i int, wid int) []byte {
	var u uint = uint(i)
	if u == 0 && wid <= 1 {
		return append(buf, '0')
	}

	// Assemble decimal in reverse order.
	var b [32]byte
	bp := len(b)
	for ; u > 0 || wid > 0; u /= 10 {
		bp--
		wid--
		b[bp] = byte(u%10) + '0'
	}
	return append(buf, b[bp:]...)
}

func (fs *fileStorage) doLog(t time.Time, str string) {
	year, month, day := t.Date()
	hour, min, sec := t.Clock()
	msec := t.Nanosecond() / 1e3
	// date
	fs.buf = itoa(fs.buf[:0], year, 4)
	fs.buf = append(fs.buf, '/')
	fs.buf = itoa(fs.buf, int(month), 2)
	fs.buf = append(fs.buf, '/')
	fs.buf = itoa(fs.buf, day, 4)
	fs.buf = append(fs.buf, ' ')
	// time
	fs.buf = itoa(fs.buf, hour, 2)
	fs.buf = append(fs.buf, ':')
	fs.buf = itoa(fs.buf, min, 2)
	fs.buf = append(fs.buf, ':')
	fs.buf = itoa(fs.buf, sec, 2)
	fs.buf = append(fs.buf, '.')
	fs.buf = itoa(fs.buf, msec, 6)
	fs.buf = append(fs.buf, ' ')
	// write
	fs.buf = append(fs.buf, []byte(str)...)
	fs.buf = append(fs.buf, '\n')
	fs.logw.Write(fs.buf)
}

func (fs *fileStorage) Log(str string) {
	t := time.Now()
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if fs.open < 0 {
		return
	}
	fs.doLog(t, str)
}

func (fs *fileStorage) log(str string) {
	fs.doLog(time.Now(), str)
}

func (fs *fileStorage) GetFile(num uint64, t FileType) File {
	return &file{fs: fs, num: num, t: t}
}

func (fs *fileStorage) GetFiles(t FileType) ([]File, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if fs.open < 0 {
		return nil, ErrClosed
	}
	dir, err := os.Open(fs.path)
	if err != nil {
		return nil, err
	}
	fnn, err := dir.Readdirnames(0)
	if err := dir.Close(); err != nil {
		fs.log(fmt.Sprintf("close dir: %v", err))
	}
	if err != nil {
		return nil, err
	}
	var ff []File
	f := &file{fs: fs}
	for _, fn := range fnn {
		if f.parse(fn) && (f.t&t) != 0 {
			ff = append(ff, f)
			f = &file{fs: fs}
		}
	}
	return ff, nil
}

func (fs *fileStorage) GetManifest() (File, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if fs.open < 0 {
		return nil, ErrClosed
	}
	path := filepath.Join(fs.path, "CURRENT")
	r, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		if e := err.(*os.PathError); e != nil {
			err = e.Err
		}
		return nil, err
	}
	defer func() {
		if err := r.Close(); err != nil {
			fs.log(fmt.Sprintf("close CURRENT: %v", err))
		}
	}()
	b, err := ioutil.ReadAll(r)
	if err != nil {
		return nil, err
	}
	f := &file{fs: fs}
	if len(b) < 1 || b[len(b)-1] != '\n' || !f.parse(string(b[:len(b)-1])) {
		return nil, errors.New("leveldb/storage: invalid CURRENT file")
	}
	return f, nil
}

func (fs *fileStorage) SetManifest(f File) (err error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if fs.open < 0 {
		return ErrClosed
	}
	f2, ok := f.(*file)
	if !ok || f2.t != TypeManifest {
		return ErrInvalidFile
	}
	defer func() {
		if err != nil {
			fs.log(fmt.Sprintf("CURRENT: %v", err))
		}
	}()
	path := fmt.Sprintf("%s.%d", filepath.Join(fs.path, "CURRENT"), f2.num)
	w, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}
	defer func() {
		if err := w.Close(); err != nil {
			fs.log(fmt.Sprintf("close CURRENT.%d: %v", f2.num, err))
		}
	}()
	_, err = fmt.Fprintln(w, f2.name())
	if err != nil {
		return err
	}
	err = rename(path, filepath.Join(fs.path, "CURRENT"))
	return
}

func (fs *fileStorage) Close() error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if fs.open < 0 {
		return ErrClosed
	}
	// Clear the finalizer.
	runtime.SetFinalizer(fs, nil)

	if fs.open > 0 {
		fs.log(fmt.Sprintf("refuse to close, %d files still open", fs.open))
		return fmt.Errorf("leveldb/storage: cannot close, %d files still open", fs.open)
	}
	fs.open = -1
	e1 := fs.logw.Close()
	err := fs.flock.release()
	if err == nil {
		err = e1
	}
	return err
}

type fileWrap struct {
	*os.File
	f *file
}

func (fw fileWrap) Close() error {
	f := fw.f
	f.fs.mu.Lock()
	defer f.fs.mu.Unlock()
	if !f.open {
		return ErrClosed
	}
	f.open = false
	f.fs.open--
	err := fw.File.Close()
	if err != nil {
		f.fs.log(fmt.Sprint("close %s.%d: %v", f.Type(), f.Num(), err))
	}
	return err
}

type file struct {
	fs   *fileStorage
	num  uint64
	t    FileType
	open bool
}

func (f *file) Open() (Reader, error) {
	f.fs.mu.Lock()
	defer f.fs.mu.Unlock()
	if f.fs.open < 0 {
		return nil, ErrClosed
	}
	if f.open {
		return nil, errFileOpen
	}
	of, err := os.OpenFile(f.path(), os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	f.open = true
	f.fs.open++
	return fileWrap{of, f}, nil
}

func (f *file) Create() (Writer, error) {
	f.fs.mu.Lock()
	defer f.fs.mu.Unlock()
	if f.fs.open < 0 {
		return nil, ErrClosed
	}
	if f.open {
		return nil, errFileOpen
	}
	of, err := os.OpenFile(f.path(), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return nil, err
	}
	f.open = true
	f.fs.open++
	return fileWrap{of, f}, nil
}

func (f *file) Type() FileType {
	return f.t
}

func (f *file) Num() uint64 {
	return f.num
}

func (f *file) Remove() error {
	f.fs.mu.Lock()
	defer f.fs.mu.Unlock()
	if f.fs.open < 0 {
		return ErrClosed
	}
	if f.open {
		return errFileOpen
	}
	err := os.Remove(f.path())
	if err != nil {
		f.fs.log(fmt.Sprint("remove %s.%d: %v", f.Type(), f.Num(), err))
	}
	return err
}

func (f *file) name() string {
	switch f.t {
	case TypeManifest:
		return fmt.Sprintf("MANIFEST-%06d", f.num)
	case TypeJournal:
		return fmt.Sprintf("%06d.log", f.num)
	case TypeTable:
		return fmt.Sprintf("%06d.sst", f.num)
	default:
		panic("invalid file type")
	}
	return ""
}

func (f *file) path() string {
	return filepath.Join(f.fs.path, f.name())
}

func (f *file) parse(name string) bool {
	var num uint64
	var tail string
	_, err := fmt.Sscanf(name, "%d.%s", &num, &tail)
	if err == nil {
		switch tail {
		case "log":
			f.t = TypeJournal
		case "sst":
			f.t = TypeTable
		default:
			return false
		}
		f.num = num
		return true
	}
	n, _ := fmt.Sscanf(name, "MANIFEST-%d%s", &num, &tail)
	if n == 1 {
		f.t = TypeManifest
		f.num = num
		return true
	}

	return false
}
