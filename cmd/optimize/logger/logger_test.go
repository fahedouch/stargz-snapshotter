/*
   Copyright The containerd Authors.

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

package logger

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hanwen/go-fuse/fuse"
	"github.com/hanwen/go-fuse/fuse/nodefs"
	"golang.org/x/sys/unix"
)

const (
	opaqueXattr      = "trusted.overlay.opaque"
	opaqueXattrValue = "y"
)

func TestExistence(t *testing.T) {
	tests := []struct {
		name string
		in   []tarent
		want []fsCheck
	}{
		{
			name: "1_whiteout_with_sibling",
			in: tarfile(
				directory("foo/"),
				regfile("foo/bar.txt", ""),
				regfile("foo/.wh.foo.txt", ""),
			),
			want: checks(
				hasValidWhiteout("foo/foo.txt"),
				fileNotExist("foo/.wh.foo.txt"),
			),
		},
		{
			name: "1_whiteout_with_duplicated_name",
			in: tarfile(
				directory("foo/"),
				regfile("foo/bar.txt", "test"),
				regfile("foo/.wh.bar.txt", ""),
			),
			want: checks(
				hasFileContents("foo/bar.txt", "test"),
				fileNotExist("foo/.wh.bar.txt"),
			),
		},
		{
			name: "1_opaque",
			in: tarfile(
				directory("foo/"),
				regfile("foo/.wh..wh..opq", ""),
			),
			want: checks(
				hasNodeXattrs("foo/", opaqueXattr, opaqueXattrValue),
				fileNotExist("foo/.wh..wh..opq"),
			),
		},
		{
			name: "1_opaque_with_sibling",
			in: tarfile(
				directory("foo/"),
				regfile("foo/.wh..wh..opq", ""),
				regfile("foo/bar.txt", "test"),
			),
			want: checks(
				hasNodeXattrs("foo/", opaqueXattr, opaqueXattrValue),
				hasFileContents("foo/bar.txt", "test"),
				fileNotExist("foo/.wh..wh..opq"),
			),
		},
		{
			name: "1_opaque_with_xattr",
			in: tarfile(
				directory("foo/", xAttr{"foo": "bar"}),
				regfile("foo/.wh..wh..opq", ""),
			),
			want: checks(
				hasNodeXattrs("foo/", opaqueXattr, opaqueXattrValue),
				hasNodeXattrs("foo/", "foo", "bar"),
				fileNotExist("foo/.wh..wh..opq"),
			),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inTar, cancelIn := buildTar(t, tt.in)
			defer cancelIn()
			inTarData, err := ioutil.ReadAll(inTar)
			if err != nil {
				t.Fatalf("failed to read input tar: %q", err)
			}
			root := newRoot(bytes.NewReader(inTarData), NewOpenReadMonitor())
			_ = nodefs.NewFileSystemConnector(root, &nodefs.Options{
				NegativeTimeout: 0,
				AttrTimeout:     time.Second,
				EntryTimeout:    time.Second,
				Owner:           nil, // preserve owners.
			})
			if err := root.InitNodes(); err != nil {
				t.Fatalf("failed to initialize nodes: %v", err)
			}
			for _, want := range tt.want {
				want.check(t, root)
			}
		})
	}
}

type fsCheck interface {
	check(t *testing.T, root *node)
}

func checks(s ...fsCheck) []fsCheck { return s }

type fsCheckFn func(*testing.T, *node)

func (f fsCheckFn) check(t *testing.T, root *node) { f(t, root) }

func fileNotExist(file string) fsCheck {
	return fsCheckFn(func(t *testing.T, root *node) {
		ent, inode, err := getDirentAndNode(root, file)
		if err == nil || ent != nil || inode != nil {
			t.Errorf("Node %q exists", file)
		}
	})
}

func hasFileContents(file string, want string) fsCheck {
	return fsCheckFn(func(t *testing.T, root *node) {
		_, inode, err := getDirentAndNode(root, file)
		if err != nil {
			t.Fatalf("failed to get node %q: %v", file, err)
		}
		n, ok := inode.Node().(*node)
		if !ok {
			t.Fatalf("entry %q isn't a normal node", file)
		}
		if n.r == nil {
			t.Fatalf("reader not found for file %q", file)
		}
		data := make([]byte, n.attr.Size)
		gotSize, err := n.r.ReadAt(data, 0)
		if uint64(gotSize) != n.attr.Size || (err != nil && err != io.EOF) {
			t.Errorf("failed to read %q: %v", file, err)
		}
		if string(data) != want {
			t.Errorf("Contents(%q) = %q, want %q", file, data, want)
		}
	})
}

func hasValidWhiteout(name string) fsCheck {
	return fsCheckFn(func(t *testing.T, root *node) {
		ent, inode, err := getDirentAndNode(root, name)
		if err != nil {
			t.Fatalf("failed to get node %q: %v", name, err)
		}
		n, ok := inode.Node().(*whiteout)
		if !ok {
			t.Fatalf("entry %q isn't a whiteout node", name)
		}
		var a fuse.Attr
		if status := n.GetAttr(&a, nil, nil); status != fuse.OK {
			t.Fatalf("failed to get attributes of file %q: %v", name, status)
		}
		if a.Ino != ent.Ino {
			t.Errorf("inconsistent inodes %d(Node) != %d(Dirent)", a.Ino, ent.Ino)
			return
		}

		// validate the direntry
		if ent.Mode&syscall.S_IFCHR != syscall.S_IFCHR {
			t.Errorf("whiteout entry %q isn't a char device %q but %q",
				name, strconv.FormatUint(uint64(syscall.S_IFCHR), 2), strconv.FormatUint(uint64(ent.Mode), 2))
			return
		}

		// validate the node
		if a.Mode&syscall.S_IFCHR != syscall.S_IFCHR {
			t.Errorf("whiteout node %q isn't a char device %q but %q",
				name, strconv.FormatUint(uint64(syscall.S_IFCHR), 2), strconv.FormatUint(uint64(a.Mode), 2))
			return
		}
		if a.Rdev != uint32(unix.Mkdev(0, 0)) {
			t.Errorf("whiteout %q has invalid device numbers (%d, %d); want (0, 0)",
				name, unix.Major(uint64(a.Rdev)), unix.Minor(uint64(a.Rdev)))
			return
		}
	})
}

func hasNodeXattrs(entry, name, value string) fsCheck {
	return fsCheckFn(func(t *testing.T, root *node) {
		_, inode, err := getDirentAndNode(root, entry)
		if err != nil {
			t.Fatalf("failed to get node %q: %v", entry, err)
		}
		n, ok := inode.Node().(*node)
		if !ok {
			t.Fatalf("entry %q isn't a normal node", entry)
		}

		// check xattr exists in the xattrs list.
		attrs, status := n.ListXAttr(nil)
		if status != fuse.OK {
			t.Fatalf("failed to get xattrs list of node %q: %v", entry, err)
		}
		var found bool
		for _, x := range attrs {
			if x == name {
				found = true
			}
		}
		if !found {
			t.Errorf("node %q doesn't have an opaque xattr %q", entry, value)
			return
		}

		// check the xattr has valid value.
		v, status := n.GetXAttr(name, nil)
		if status != fuse.OK {
			t.Fatalf("failed to get xattr %q of node %q: %v", name, entry, err)
		}
		if string(v) != value {
			t.Errorf("node %q has an invalid xattr %q; want %q", entry, v, value)
			return
		}
	})
}

// getDirentAndNode gets dirent and node at the specified path at once and makes
// sure that the both of them exist.
func getDirentAndNode(root *node, path string) (ent *fuse.DirEntry, n *nodefs.Inode, err error) {
	dir, base := filepath.Split(filepath.Clean(path))

	// get the target's parent directory.
	var attr fuse.Attr
	d := root
	for _, name := range strings.Split(dir, "/") {
		if len(name) == 0 {
			continue
		}
		di, status := d.Lookup(&attr, name, nil)
		if status != fuse.OK {
			err = fmt.Errorf("failed to lookup directory %q: %v", name, status)
			return
		}
		var ok bool
		if d, ok = di.Node().(*node); !ok {
			err = fmt.Errorf("directory %q isn't a normal node", name)
			return
		}

	}

	// get the target's direntry.
	var ents []fuse.DirEntry
	ents, status := d.OpenDir(nil)
	if status != fuse.OK {
		err = fmt.Errorf("failed to open directory %q: %v", path, status)
	}
	var found bool
	for _, e := range ents {
		if e.Name == base {
			ent, found = &e, true
			break
		}
	}
	if !found {
		err = fmt.Errorf("direntry %q not found in the parent directory of %q", base, path)
	}

	// get the target's node.
	n, status = d.Lookup(&attr, base, nil)
	if status != fuse.OK {
		err = fmt.Errorf("failed to lookup node %q: %v", path, status)
	}

	return
}

func TestOpenRead(t *testing.T) {
	tests := []struct {
		name string
		in   []tarent
		do   accessFunc
		want []string
	}{
		{
			name: "noopt",
			in: tarfile(
				regfile("foo.txt", "foo"),
				directory("bar/"),
				regfile("bar/baz.txt", "baz"),
				regfile("bar/bar.txt", "bar"),
				regfile("bar/baa.txt", "baa"),
			),
			do:   doAccess(),
			want: []string{},
		},
		{
			name: "open_and_read",
			in: tarfile(
				regfile("foo.txt", "foo"),
				directory("bar/"),
				regfile("bar/baz.txt", "baz"),
				regfile("bar/bar.txt", "bar"),
				regfile("bar/baa.txt", "baa"),
			),
			do: doAccess(
				openFile("bar/baa.txt"),
				readFile("bar/baz.txt", make([]byte, 3)),
			),
			want: []string{
				"bar/baa.txt", // open
				"bar/baz.txt", // open for read
				"bar/baz.txt", // read
			},
		},
		{
			name: "hardlink",
			in: tarfile(
				regfile("foo.txt", "foo"),
				regfile("baz.txt", "baz"),
				hardlink("bar.txt", "baz.txt"),
				regfile("baa.txt", "baa"),
			),
			do: doAccess(
				readFile("bar.txt", make([]byte, 3)),
			),
			want: []string{
				"baz.txt", // open for read; must be original file
				"baz.txt", // read; must be original file
			},
		},
		{
			name: "symlink",
			in: tarfile(
				regfile("foo.txt", "foo"),
				regfile("baz.txt", "baz"),
				symlink("bar.txt", "baz.txt"),
				regfile("baa.txt", "baa"),
			),
			do: doAccess(
				readFile("bar.txt", make([]byte, 3)),
			),
			want: []string{
				"baz.txt", // open for read; must be original file
				"baz.txt", // read; must be original file
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Prepare input tar file
			inTar, cancelIn := buildTar(t, tt.in)
			defer cancelIn()
			inTarData, err := ioutil.ReadAll(inTar)
			if err != nil {
				t.Fatalf("failed to read input tar: %q", err)
			}

			dir, err := ioutil.TempDir("", "loggertest")
			if err != nil {
				t.Fatalf("failed to prepare temp directory")
			}
			defer os.RemoveAll(dir)

			monitor := NewOpenReadMonitor()
			cleanup, err := Mount(dir, bytes.NewReader(inTarData), monitor)
			if err != nil {
				t.Fatalf("failed to mount at %q: %q", dir, err)
			}
			defer cleanup()

			if err := tt.do(dir); err != nil {
				t.Fatalf("failed to do specified operations: %q", err)
			}

			if err := cleanup(); err != nil {
				t.Logf("failed to unmount: %v", err)
			}

			log := monitor.DumpLog()
			for i, l := range log {
				t.Logf("  [%d]: %s", i, l)
			}
			if len(log) != len(tt.want) {
				t.Errorf("num of log: got %d; want %d", len(log), len(tt.want))
				return
			}
			for i, l := range log {
				if l != tt.want[i] {
					t.Errorf("log: got %q; want %q", l, tt.want[i])
					return
				}
			}
		})
	}
}

func buildTar(t *testing.T, ents []tarent) (r io.Reader, cancel func()) {
	pr, pw := io.Pipe()
	go func() {
		tw := tar.NewWriter(pw)
		for _, ent := range ents {
			if err := tw.WriteHeader(ent.header); err != nil {
				t.Errorf("writing header to the input tar: %v", err)
				pw.Close()
				return
			}
			if _, err := tw.Write(ent.contents); err != nil {
				t.Errorf("writing contents to the input tar: %v", err)
				pw.Close()
				return
			}
		}
		if err := tw.Close(); err != nil {
			t.Errorf("closing write of input tar: %v", err)
		}
		pw.Close()
	}()
	return pr, func() { go pr.Close(); go pw.Close() }
}

type accessFunc func(basepath string) error

func doAccess(ac ...accessFunc) accessFunc {
	return func(basepath string) error {
		for _, a := range ac {
			if err := a(basepath); err != nil {
				return err
			}
		}
		return nil
	}
}

func openFile(filename string) accessFunc {
	return func(basepath string) error {
		f, err := os.Open(filepath.Join(basepath, filename))
		if err != nil {
			return err
		}
		f.Close()
		return nil
	}
}

func readFile(filename string, b []byte) accessFunc {
	return func(basepath string) error {
		f, err := os.Open(filepath.Join(basepath, filename))
		if err != nil {
			return err
		}
		defer f.Close()
		if _, err := f.Read(b); err != nil {
			if err != io.EOF {
				return err
			}
		}
		return nil
	}
}

type tarent struct {
	header   *tar.Header
	contents []byte
}

func tarfile(es ...entry) (res []tarent) {
	for _, e := range es {
		res = e(res)
	}

	return
}

type entry func([]tarent) []tarent

func regfile(name string, contents string) entry {
	return func(in []tarent) []tarent {
		return append(in, tarent{
			header: &tar.Header{
				Typeflag: tar.TypeReg,
				Name:     name,
				Mode:     0644,
				Size:     int64(len(contents)),
			},
			contents: []byte(contents),
		})
	}
}

type xAttr map[string]string

func directory(name string, opts ...interface{}) entry {
	if !strings.HasSuffix(name, "/") {
		panic(fmt.Sprintf("dir %q hasn't suffix /", name))
	}
	var xattrs xAttr
	for _, opt := range opts {
		if v, ok := opt.(xAttr); ok {
			xattrs = v
		}
	}
	return func(in []tarent) []tarent {
		return append(in, tarent{
			header: &tar.Header{
				Typeflag: tar.TypeDir,
				Name:     name,
				Mode:     0644,
				Xattrs:   xattrs,
			},
		})
	}
}

func hardlink(name string, linkname string) entry {
	return func(in []tarent) []tarent {
		return append(in, tarent{
			header: &tar.Header{
				Typeflag: tar.TypeLink,
				Name:     name,
				Mode:     0644,
				Linkname: linkname,
			},
		})
	}
}

func symlink(name string, linkname string) entry {
	return func(in []tarent) []tarent {
		return append(in, tarent{
			header: &tar.Header{
				Typeflag: tar.TypeSymlink,
				Name:     name,
				Mode:     0644,
				Linkname: linkname,
			},
		})
	}
}
