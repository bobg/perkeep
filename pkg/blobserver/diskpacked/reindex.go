/*
Copyright 2013 Google Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

     http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package diskpacked

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"strconv"

	"camlistore.org/pkg/blob"
	"camlistore.org/pkg/context"
	"camlistore.org/pkg/sorted"
	"camlistore.org/pkg/sorted/kvfile"
)

var camliDebug, _ = strconv.ParseBool(os.Getenv("CAMLI_DEBUG"))

// Reindex rewrites the index files of the diskpacked .pack files
func Reindex(root string, overwrite bool) (err error) {
	// there is newStorage, but that may open a file for writing
	var s = &storage{root: root}
	index, err := kvfile.NewStorage(filepath.Join(root, indexKV))
	if err != nil {
		return err
	}
	defer func() {
		closeErr := index.Close()
		// just returning the first error - if the index or disk is corrupt
		// and can't close, it's very likely these two errors are related and
		// have the same root cause.
		if err == nil {
			err = closeErr
		}
	}()

	ctx := context.TODO() // TODO(tgulacsi): get the verbosity from context
	for i := 0; i >= 0; i++ {
		fh, err := os.Open(s.filename(i))
		if err != nil {
			if os.IsNotExist(err) {
				break
			}
			return err
		}
		err = s.reindexOne(ctx, index, overwrite, i)
		fh.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *storage) reindexOne(ctx *context.Context, index sorted.KeyValue, overwrite bool, packID int) error {

	var batch sorted.BatchMutation
	if overwrite {
		batch = index.BeginBatch()
	}
	allOk := true

	// TODO(tgulacsi): proper verbose from context
	verbose := camliDebug
	err := s.walkPack(verbose, packID,
		func(packID int, ref blob.Ref, offset int64, size uint32) error {
			if !ref.Valid() {
				if camliDebug {
					log.Printf("found deleted blob in %d at %d with size %d", packID, offset, size)
				}
				return nil
			}
			meta := blobMeta{packID, offset, size}.String()
			if overwrite && batch != nil {
				batch.Set(ref.String(), meta)
			} else {
				if old, err := index.Get(ref.String()); err != nil {
					allOk = false
					if err == sorted.ErrNotFound {
						log.Println(ref.String() + ": cannot find in index!")
					} else {
						log.Println(ref.String()+": error getting from index: ", err.Error())
					}
				} else if old != meta {
					allOk = false
					log.Printf("%s: index mismatch - index=%s data=%s", ref.String(), old, meta)
				}
			}
			return nil
		})
	if err != nil {
		return err
	}

	if overwrite && batch != nil {
		log.Printf("overwriting %s from %d", index, packID)
		if err = index.CommitBatch(batch); err != nil {
			return err
		}
	} else if !allOk {
		return fmt.Errorf("index does not match data in %d", packID)
	}
	return nil
}

// Walk walks the storage and calls the walker callback with each blobref
// stops if walker returns non-nil error, and returns that
func (s *storage) Walk(ctx *context.Context,
	walker func(packID int, ref blob.Ref, offset int64, size uint32) error) error {

	// TODO(tgulacsi): proper verbose flag from context
	verbose := camliDebug

	for i := 0; i >= 0; i++ {
		fh, err := os.Open(s.filename(i))
		if err != nil {
			if os.IsNotExist(err) {
				break
			}
			return err
		}
		fh.Close()
		if err = s.walkPack(verbose, i, walker); err != nil {
			return err
		}
	}
	return nil
}

// walkPack walks the given pack and calls the walker callback with each blobref.
// Stops if walker returns non-nil error and returns that.
func (s *storage) walkPack(verbose bool, packID int,
	walker func(packID int, ref blob.Ref, offset int64, size uint32) error) error {

	fh, err := os.Open(s.filename(packID))
	if err != nil {
		return err
	}
	defer fh.Close()
	name := fh.Name()

	var (
		pos  int64
		size uint32
		ref  blob.Ref
	)

	errAt := func(prefix, suffix string) error {
		if prefix != "" {
			prefix = prefix + " "
		}
		if suffix != "" {
			suffix = " " + suffix
		}
		return fmt.Errorf(prefix+"at %d (0x%x) in %q:"+suffix, pos, pos, name)
	}

	br := bufio.NewReaderSize(fh, 512)
	for {
		if b, err := br.ReadByte(); err != nil {
			if err == io.EOF {
				break
			}
			return errAt("error while reading", err.Error())
		} else if b != '[' {
			return errAt(fmt.Sprintf("found byte 0x%x", b), "but '[' should be here!")
		}
		chunk, err := br.ReadSlice(']')
		if err != nil {
			if err == io.EOF {
				break
			}
			return errAt("error reading blob header", err.Error())
		}
		m := len(chunk)
		chunk = chunk[:m-1]
		i := bytes.IndexByte(chunk, byte(' '))
		if i <= 0 {
			return errAt("", fmt.Sprintf("bad header format (no space in %q)", chunk))
		}
		size64, err := strconv.ParseUint(string(chunk[i+1:]), 10, 32)
		if err != nil {
			return errAt(fmt.Sprintf("cannot parse size %q as int", chunk[i+1:]), err.Error())
		}
		size = uint32(size64)

		if deletedBlobRef.Match(chunk[:i]) {
			ref = blob.Ref{}
			if verbose {
				log.Printf("found deleted at %d", pos)
			}
		} else {
			var ok bool
			ref, ok = blob.Parse(string(chunk[:i]))
			if !ok {
				return errAt("", fmt.Sprintf("cannot parse %q as blobref", chunk[:i]))
			}
			if verbose {
				log.Printf("found %s at %d", ref, pos)
			}
		}
		if err = walker(packID, ref, pos+1+int64(m), size); err != nil {
			return err
		}

		pos += 1 + int64(m)
		// TODO(tgulacsi): not just seek, but check the hashes of the files
		// maybe with a different command-line flag, only.
		if pos, err = fh.Seek(pos+int64(size), 0); err != nil {
			return errAt("", "cannot seek +"+strconv.FormatUint(size64, 10)+" bytes")
		}
		// drain the buffer after the underlying reader Seeks
		io.CopyN(ioutil.Discard, br, int64(br.Buffered()))
	}
	return nil
}
