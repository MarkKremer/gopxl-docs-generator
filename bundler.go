package main

import (
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

type Context struct {
	bundle  *Bundle
	mapping *Mapping
}

func (c *Context) GetUriSegment(i int) string {
	p := strings.Split(c.mapping.dstPath, "/")
	if i >= len(p) {
		return ""
	}
	return p[i]
}

func (c *Context) ToAbsUrl(file string) *url.URL {
	return c.bundle.rootUrl.JoinPath(file)
}

// RewriteContentUrl rewrites the link so that it points to the new
// location specified in the bundle.
func (c *Context) RewriteContentUrl(link string) (string, error) {
	u, err := url.Parse(link)
	if err != nil {
		return "", fmt.Errorf("cannot parse url %s: %w", link, err)
	}
	if u.IsAbs() {
		return link, nil
	}
	var srcPath string
	if len(u.Path) > 0 && u.Path[0] == '/' {
		// relative to repository root
		srcPath = filepath.Clean(strings.TrimLeft(u.Path, "/"))
	} else {
		// relative to current file
		srcPath = filepath.Join(filepath.Dir(c.mapping.srcPath), u.Path)
	}
	f, ok := c.bundle.srcPaths[c.mapping.srcFs][srcPath]
	if !ok {
		return "", fs.ErrNotExist
	}
	return c.ToAbsUrl(f.dstPath).String(), nil
}

type Bundler struct {
	rules     []*MappingRule
	redirects []*RedirectRule
}

func NewBundler() *Bundler {
	return &Bundler{}
}

func (b *Bundler) FromFs(fs fs.FS) *MappingRule {
	r := &MappingRule{
		srcFs:    fs,
		compiler: &copyCompiler{},
	}
	b.rules = append(b.rules, r)
	return r
}

func (b *Bundler) Redirect(from, to *url.URL) *RedirectRule {
	r := &RedirectRule{
		from: from,
		to:   to,
	}
	b.redirects = append(b.redirects, r)
	return r
}

func (b *Bundler) Compile(rootUrl *url.URL) (*Bundle, error) {
	bun := Bundle{
		rootUrl:      rootUrl,
		files:        nil,
		srcPaths:     make(map[fs.FS]map[string]*Mapping),
		dstPaths:     make(map[string]*Mapping),
		redirects:    nil,
		redirectsMap: make(map[string]*redirect),
	}
	for _, r := range b.rules {
		srcPaths, err := r.sourceFiles()
		if err != nil {
			return nil, err
		}
		for _, srcPath := range srcPaths {
			destPath, err := r.DestPath(srcPath)
			if err != nil {
				return nil, err
			}
			bun.files = append(bun.files, Mapping{
				srcFs:    r.srcFs,
				srcPath:  srcPath,
				dstPath:  destPath,
				compiler: r.compiler,
			})
		}
	}
	sort.Slice(bun.files, func(i, j int) bool {
		return bun.files[i].dstPath < bun.files[j].dstPath
	})
	for i, f := range bun.files {
		if _, ok := bun.srcPaths[f.srcFs]; !ok {
			bun.srcPaths[f.srcFs] = make(map[string]*Mapping)
		}
		bun.srcPaths[f.srcFs][f.srcPath] = &bun.files[i]
		bun.dstPaths[f.dstPath] = &bun.files[i]
	}
	bun.redirects = make([]redirect, len(b.redirects))
	for i, r := range b.redirects {
		from, err := r.compileFrom()
		if err != nil {
			return nil, err
		}
		to, err := r.compileTo(&bun)
		if err != nil {
			return nil, err
		}
		cr := redirect{
			from: from,
			to:   to,
		}
		bun.redirects[i] = cr
		bun.redirectsMap[cr.from] = &bun.redirects[i]
	}
	return &bun, nil
}

type RedirectRule struct {
	from   *url.URL
	to     *url.URL
	toFs   fs.FS // for local redirects
	dstDir string
}

func (r *RedirectRule) PutInDir(dir string) *RedirectRule {
	r.dstDir = dir
	return r
}

func (r *RedirectRule) WithTargetFs(fs fs.FS) *RedirectRule {
	r.toFs = fs
	return r
}

func (r *RedirectRule) compileFrom() (string, error) {
	if r.from.IsAbs() {
		return "", errors.New(fmt.Sprintf("the path to redirect must be a local path, was %s", r.from.String()))
	}
	from := filepath.Join(r.dstDir, r.from.Path)
	if filepath.Ext(from) != ".html" {
		from = filepath.Join(from, "index.html")
	}
	return from, nil
}

func (r *RedirectRule) compileTo(bun *Bundle) (*url.URL, error) {
	if r.to.IsAbs() || r.toFs == nil {
		return r.to, nil
	}
	srcPath := filepath.Clean(strings.TrimLeft(r.to.Path, "/"))
	f, ok := bun.srcPaths[r.toFs][srcPath]
	if !ok {
		return nil, fmt.Errorf("redirect destination %s not found: %w", r.to, fs.ErrNotExist)
	}
	return bun.rootUrl.JoinPath(f.dstPath), nil
}

type srcFilesLister func(filesystem fs.FS) ([]string, error)
type srcFileFilter func(file string) bool

type MappingRule struct {
	srcFs          fs.FS
	srcDir         string
	srcFilesLister srcFilesLister
	srcFileFilter  srcFileFilter
	dstDir         string
	compiler       Compiler
}

func (b *MappingRule) TakeFile(file string) *MappingRule {
	b.srcDir = filepath.Dir(file)
	b.srcFilesLister = func(filesystem fs.FS) ([]string, error) {
		_, err := filesystem.Open(file)
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("file %s does not exist: %w", file, err)
		}
		return []string{file}, nil
	}
	return b
}

func (b *MappingRule) TakeDir(dir string) *MappingRule {
	return b.TakeGlob(dir, "**/*")
}

func (b *MappingRule) TakeGlob(dir string, glob string) *MappingRule {
	b.srcDir = dir
	b.srcFilesLister = func(filesystem fs.FS) ([]string, error) {
		fullGlob := filepath.Join(dir, glob)
		matches, err := doublestar.Glob(
			filesystem,
			fullGlob,
			doublestar.WithFilesOnly(),
			doublestar.WithFailOnIOErrors(),
			doublestar.WithFailOnPatternNotExist(),
		)
		if err != nil {
			return nil, fmt.Errorf("could not read files with glob %v: %w", fullGlob, err)
		}
		return matches, nil
	}
	return b
}

func (b *MappingRule) Filter(filter srcFileFilter) *MappingRule {
	b.srcFileFilter = filter
	return b
}

func (b *MappingRule) CompileWith(compiler Compiler) *MappingRule {
	b.compiler = compiler
	return b
}

func (b *MappingRule) PutInDir(dir string) {
	b.dstDir = dir
}

func (b *MappingRule) sourceFiles() ([]string, error) {
	files, err := b.srcFilesLister(b.srcFs)
	if err != nil {
		return nil, err
	}
	if b.srcFileFilter == nil {
		return files, nil
	}
	var filtered []string
	for _, file := range files {
		if b.srcFileFilter(file) {
			filtered = append(filtered, file)
		}
	}
	return filtered, nil
}

func (b *MappingRule) DestPath(srcPath string) (string, error) {
	rel, err := filepath.Rel(b.srcDir, srcPath)
	if err != nil {
		return "", fmt.Errorf("could not get relative path of file %v to source directory %v: %w", srcPath, b.srcDir, err)
	}
	return filepath.Join(
		b.dstDir,
		filepath.Dir(rel),
		b.compiler.OutputFileName(filepath.Base(rel)),
	), nil
}

type Compiler interface {
	OutputFileName(oldName string) (newName string)
	Compile(dst io.Writer, src io.Reader, request *Context) error
}

type copyCompiler struct{}

func (c *copyCompiler) OutputFileName(oldName string) (newName string) {
	return oldName
}

func (c *copyCompiler) Compile(dst io.Writer, src io.Reader, request *Context) error {
	_, err := io.Copy(dst, src)
	return err
}

type Bundle struct {
	rootUrl      *url.URL
	files        []Mapping
	srcPaths     map[fs.FS]map[string]*Mapping
	dstPaths     map[string]*Mapping
	redirects    []redirect
	redirectsMap map[string]*redirect // [from]redirect
}

type Mapping struct {
	srcFs    fs.FS
	srcPath  string
	dstPath  string
	compiler Compiler
}

type redirect struct {
	from string
	to   *url.URL
}

func (m *Mapping) Open() (fs.File, error) {
	return m.srcFs.Open(m.srcPath)
}

func (bun *Bundle) DestFiles() []string {
	files := make([]string, len(bun.files))
	for _, f := range bun.files {
		files = append(files, f.dstPath)
	}
	return files
}

func (bun *Bundle) CompileAllToDir(dir string) error {
	for i := range bun.files {
		if err := bun.compileFileToDir(&bun.files[i], dir); err != nil {
			return err
		}
	}
	for i := range bun.redirects {
		if err := bun.compileRedirectToDir(&bun.redirects[i], dir); err != nil {
			return err
		}
	}
	return nil
}

func (bun *Bundle) compileFileToDir(f *Mapping, dir string) error {
	absDstPath := filepath.Join(dir, f.dstPath)

	err := os.MkdirAll(filepath.Dir(absDstPath), 0755)
	if err != nil {
		return fmt.Errorf("could not create directory for output file %v: %w", absDstPath, err)
	}
	dst, err := os.OpenFile(absDstPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return fmt.Errorf("could not open output file %v: %w", dir, err)
	}
	defer dst.Close()

	return bun.compileFileToWriter(f, dst)
}

func (bun *Bundle) compileRedirectToDir(r *redirect, dir string) error {
	absDstPath := filepath.Join(dir, r.from)

	err := os.MkdirAll(filepath.Dir(absDstPath), 0755)
	if err != nil {
		return fmt.Errorf("could not create directory for output file %v: %w", absDstPath, err)
	}
	dst, err := os.OpenFile(absDstPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return fmt.Errorf("could not open output file %v: %w", dir, err)
	}
	defer dst.Close()

	return bun.compileRedirectToWriter(r, dst)
}

// CompileFileToWriter accepts the destination path pth of a file and writes
// its compiled contents to writer w.
func (bun *Bundle) CompileFileToWriter(pth string, w io.Writer) error {
	pth = path.Clean(pth)
	if f, ok := bun.dstPaths[pth]; ok {
		return bun.compileFileToWriter(f, w)
	}
	if r, ok := bun.redirectsMap[pth]; ok {
		return bun.compileRedirectToWriter(r, w)
	}
	return fs.ErrNotExist
}

func (bun *Bundle) compileFileToWriter(f *Mapping, w io.Writer) error {
	src, err := f.Open()
	if err != nil {
		return fmt.Errorf("could not open source file %s: %w", f.srcPath, err)
	}
	defer src.Close()
	err = f.compiler.Compile(w, src, &Context{
		mapping: f,
		bundle:  bun,
	})
	if err != nil {
		return fmt.Errorf("could not compile file %v: %w", f.srcPath, err)
	}
	return nil
}

func (bun *Bundle) compileRedirectToWriter(r *redirect, w io.Writer) error {
	const file = "resources/views/redirect.gohtml"
	t, err := template.ParseFS(embeddedFs, file)
	if err != nil {
		return fmt.Errorf("could not parse redirect template %s: %w", file, err)
	}

	err = t.Execute(w, struct {
		RedirectUrl string
	}{
		RedirectUrl: r.to.String(),
	})
	if err != nil {
		return fmt.Errorf("error executing redirect template: %w", err)
	}
	return nil
}
