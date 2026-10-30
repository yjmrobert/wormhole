// Command docsite renders the Markdown under docs/ into the Wormhole
// documentation site: one toolchain, no Node. The home page opens on a neon
// horizon (a wireframe floor bent into a well under the wormhole) and the
// reading pages stay a calm navy document. Run `go run ./tools/docsite -h`
// for flags.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	chromahtml "github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/yuin/goldmark"
	highlighting "github.com/yuin/goldmark-highlighting/v2"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer/html"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// Sheet is one page of the site: a Markdown source and where it lands.
type Sheet struct {
	Slug    string // "" for the home sheet
	Source  string // path relative to the repo root
	Label   string // short name in the drawing index
	Summary string // one line under the name in the index
	// Skill marks the sheet whose source is SKILL.md with frontmatter.
	Skill bool
	// Title overrides the source's H1 when set.
	Title string
}

// sheets is the drawing index, in order. Numbers in the title block come
// from the position here.
var sheets = []Sheet{
	{Slug: "", Source: "docs/index.md", Label: "Overview", Summary: "What a wormhole is and how a question travels"},
	{Slug: "install", Source: "docs/install.md", Label: "Install and configure", Summary: "The bus, the daemon, the skill"},
	{Slug: "usage", Source: "docs/usage.md", Label: "Using it", Summary: "Commands, delivery modes, kill switches"},
	{Slug: "design", Source: "docs/design.md", Label: "Design", Summary: "Architecture, envelope, security model"},
	{Slug: "roadmap", Source: "docs/roadmap.md", Label: "Roadmap", Summary: "Milestones M0 to M5 and their gates"},
	{Slug: "m0-spike", Source: "docs/m0-spike.md", Label: "M0 spike", Summary: "The questions only real machines can answer"},
	{Slug: "skill", Source: "skills/wormhole/SKILL.md", Label: "The skill", Summary: "What the agent reads", Skill: true, Title: "The Wormhole skill"},
}

// Heading is one entry of a sheet's in-page contents.
type Heading struct {
	ID    string `json:"id"`
	Text  string `json:"text"`
	Level int    `json:"level"`
}

// Rendered is a sheet after Markdown processing.
type Rendered struct {
	Sheet
	Number   int
	Title    string
	Lede     template.HTML // first paragraph, home sheet only
	Body     template.HTML
	Headings []Heading
	Text     string
	URL      string
}

// Site is what every template sees.
type Site struct {
	Base      string
	Repo      string
	Version   string
	Built     string
	Sheets    []*Rendered
	Total     int
	StarField template.HTML
	Hero      template.HTML // the home page opener, drawn once at build time
}

type pageData struct {
	Site
	Page *Rendered
	Prev *Rendered
	Next *Rendered
	Home bool
}

func main() {
	root := flag.String("root", ".", "repository root")
	out := flag.String("out", "site", "output directory")
	base := flag.String("base", "/", "URL path the site is served under, with leading and trailing slash")
	version := flag.String("version", "dev", "release tag shown in the title block")
	repo := flag.String("repo", "https://github.com/yjmrobert/wormhole", "repository URL")
	flag.Parse()
	if err := build(*root, *out, *base, *version, *repo, time.Now()); err != nil {
		log.Fatal(err)
	}
}

// build renders every page and copies the assets.
func build(root, out, base, version, repo string, now time.Time) error {
	if !strings.HasPrefix(base, "/") || !strings.HasSuffix(base, "/") {
		return fmt.Errorf("base %q must start and end with /", base)
	}
	site := Site{Base: base, Repo: repo, Version: version, Built: now.UTC().Format("2006-01-02"), Total: len(sheets), StarField: starField(42), Hero: hero()}
	for i, s := range sheets {
		src, err := os.ReadFile(filepath.Join(root, s.Source))
		if err != nil {
			return err
		}
		if s.Skill {
			src = stripFrontmatter(src)
		}
		r, err := render(src, s, base)
		if err != nil {
			return fmt.Errorf("%s: %w", s.Source, err)
		}
		r.Number = i + 1
		site.Sheets = append(site.Sheets, r)
	}
	tpl, err := template.New("").Funcs(template.FuncMap{
		"pad": func(n int) string { return fmt.Sprintf("%02d", n) },
	}).ParseFS(templates, "templates/*.html")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	for i, r := range site.Sheets {
		pd := pageData{Site: site, Page: r, Home: r.Slug == ""}
		if i > 0 {
			pd.Prev = site.Sheets[i-1]
		}
		if i+1 < len(site.Sheets) {
			pd.Next = site.Sheets[i+1]
		}
		dir := filepath.Join(out, r.Slug)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		var buf bytes.Buffer
		if err := tpl.ExecuteTemplate(&buf, "page.html", pd); err != nil {
			return fmt.Errorf("render %s: %w", r.Source, err)
		}
		if err := os.WriteFile(filepath.Join(dir, "index.html"), buf.Bytes(), 0o644); err != nil {
			return err
		}
	}
	var buf bytes.Buffer
	if err := tpl.ExecuteTemplate(&buf, "404.html", pageData{Site: site}); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(out, "404.html"), buf.Bytes(), 0o644); err != nil {
		return err
	}
	if err := writeSearchIndex(filepath.Join(out, "search.json"), site); err != nil {
		return err
	}
	if err := copyAssets(filepath.Join(out, "assets")); err != nil {
		return err
	}
	// GitHub Pages must not run Jekyll over the output.
	return os.WriteFile(filepath.Join(out, ".nojekyll"), nil, 0o644)
}

var frontmatterRE = regexp.MustCompile(`(?s)\A---\n.*?\n---\n`)

func stripFrontmatter(b []byte) []byte {
	return frontmatterRE.ReplaceAll(b, nil)
}

// linkRewriter turns links to sibling Markdown files into site URLs.
type linkRewriter struct{ base string }

func (l linkRewriter) Transform(doc *ast.Document, reader text.Reader, pc parser.Context) {
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		link, ok := n.(*ast.Link)
		if !ok {
			return ast.WalkContinue, nil
		}
		dest := string(link.Destination)
		if strings.Contains(dest, "://") || strings.HasPrefix(dest, "#") || strings.HasPrefix(dest, "/") {
			return ast.WalkContinue, nil
		}
		path, frag, _ := strings.Cut(dest, "#")
		if !strings.HasSuffix(path, ".md") {
			return ast.WalkContinue, nil
		}
		slug := strings.TrimSuffix(filepath.Base(path), ".md")
		if slug == "index" {
			slug = ""
		}
		u := l.base + slug
		if slug != "" {
			u += "/"
		}
		if frag != "" {
			u += "#" + frag
		}
		link.Destination = []byte(u)
		return ast.WalkContinue, nil
	})
}

func newMarkdown(base string) goldmark.Markdown {
	return goldmark.New(
		goldmark.WithExtensions(
			extension.GFM,
			extension.Typographer,
			highlighting.NewHighlighting(
				highlighting.WithFormatOptions(chromahtml.WithClasses(true)),
			),
		),
		goldmark.WithParserOptions(
			parser.WithAutoHeadingID(),
			parser.WithASTTransformers(util.Prioritized(linkRewriter{base: base}, 100)),
		),
		goldmark.WithRendererOptions(html.WithUnsafe()),
	)
}

// render parses one sheet. The first H1 becomes the sheet title and is
// removed from the body; on the home sheet the first paragraph becomes
// the lede so the template can set it beside the primary actions.
func render(src []byte, s Sheet, base string) (*Rendered, error) {
	md := newMarkdown(base)
	doc := md.Parser().Parse(text.NewReader(src))
	r := &Rendered{Sheet: s, Headings: []Heading{}}
	var ledeNode ast.Node
	for n := doc.FirstChild(); n != nil; n = n.NextSibling() {
		if h, ok := n.(*ast.Heading); ok && h.Level == 1 && r.Title == "" {
			r.Title = plainText(h, src)
			next := n.NextSibling()
			doc.RemoveChild(doc, n)
			if s.Slug == "" {
				if p, ok := next.(*ast.Paragraph); ok {
					ledeNode = p
				}
			}
			break
		}
	}
	if s.Title != "" {
		r.Title = s.Title
	}
	if r.Title == "" {
		return nil, fmt.Errorf("no H1 title")
	}
	if ledeNode != nil {
		doc.RemoveChild(doc, ledeNode)
		var buf bytes.Buffer
		para := ast.NewDocument()
		para.AppendChild(para, ledeNode)
		if err := md.Renderer().Render(&buf, src, para); err != nil {
			return nil, err
		}
		r.Lede = template.HTML(strings.TrimSpace(buf.String()))
	}
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		if h, ok := n.(*ast.Heading); ok && (h.Level == 2 || h.Level == 3) {
			id, _ := h.AttributeString("id")
			r.Headings = append(r.Headings, Heading{ID: fmt.Sprint(bytesOf(id)), Text: plainText(h, src), Level: h.Level})
		}
		return ast.WalkContinue, nil
	})
	var buf bytes.Buffer
	if err := md.Renderer().Render(&buf, src, doc); err != nil {
		return nil, err
	}
	body := strings.ReplaceAll(buf.String(), "<table>", `<div class="table-scroll"><table>`)
	body = strings.ReplaceAll(body, "</table>", "</table></div>")
	r.Body = template.HTML(body)
	r.Text = strings.Join(strings.Fields(plainText(doc, src)), " ")
	r.URL = base + s.Slug
	if s.Slug != "" {
		r.URL += "/"
	}
	return r, nil
}

func bytesOf(v any) string {
	switch b := v.(type) {
	case []byte:
		return string(b)
	case string:
		return b
	}
	return ""
}

// plainText concatenates the text nodes under n.
func plainText(n ast.Node, src []byte) string {
	var b strings.Builder
	_ = ast.Walk(n, func(c ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch t := c.(type) {
		case *ast.Text:
			b.Write(t.Segment.Value(src))
			if t.SoftLineBreak() || t.HardLineBreak() {
				b.WriteByte(' ')
			}
		case *ast.String:
			b.Write(t.Value)
		case *ast.CodeSpan:
			// children are Text nodes; handled above
		case *ast.FencedCodeBlock, *ast.CodeBlock:
			lines := c.Lines()
			for i := 0; i < lines.Len(); i++ {
				seg := lines.At(i)
				b.Write(seg.Value(src))
			}
			b.WriteByte(' ')
		}
		if c.Kind() == ast.KindParagraph || c.Kind() == ast.KindHeading || c.Kind() == ast.KindListItem {
			b.WriteByte(' ')
		}
		return ast.WalkContinue, nil
	})
	return strings.TrimSpace(b.String())
}

type searchEntry struct {
	Title    string    `json:"title"`
	URL      string    `json:"url"`
	Number   int       `json:"number"`
	Headings []Heading `json:"headings"`
	Text     string    `json:"text"`
}

func writeSearchIndex(path string, site Site) error {
	var entries []searchEntry
	for _, r := range site.Sheets {
		entries = append(entries, searchEntry{Title: r.Title, URL: r.URL, Number: r.Number, Headings: r.Headings, Text: r.Text})
	}
	b, err := json.Marshal(entries)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

func copyAssets(dst string) error {
	return fs.WalkDir(assets, "assets", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(p, "assets")
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, err := assets.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o644)
	})
}

// starField draws the sparse sky behind the dark sheet: three layers of
// stars with slightly different sizes so they can drift at different
// speeds. Deterministic so builds are reproducible.
func starField(seed int64) template.HTML {
	rng := rand.New(rand.NewSource(seed))
	var b strings.Builder
	b.WriteString(`<svg class="stars" viewBox="0 0 1600 1600" preserveAspectRatio="xMidYMid slice" aria-hidden="true" focusable="false">`)
	layers := []struct {
		n    int
		rmin float64
		rmax float64
	}{{130, 0.6, 1.0}, {80, 1.0, 1.6}, {36, 1.6, 2.4}}
	for i, l := range layers {
		fmt.Fprintf(&b, `<g class="stars-l%d">`, i+1)
		for j := 0; j < l.n; j++ {
			x := rng.Float64() * 1600
			y := rng.Float64() * 1600
			r := l.rmin + rng.Float64()*(l.rmax-l.rmin)
			o := 0.35 + rng.Float64()*0.55
			fmt.Fprintf(&b, `<circle cx="%.0f" cy="%.0f" r="%.1f" opacity="%.2f"/>`, x, y, r, o)
			// The layer repeats one tile height down so the drift wraps.
			fmt.Fprintf(&b, `<circle cx="%.0f" cy="%.0f" r="%.1f" opacity="%.2f"/>`, x, y+1600, r, o)
		}
		b.WriteString(`</g>`)
	}
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}

// hero draws the home page opener: a wireframe floor in perspective that sags
// into a well under the wormhole on the horizon, with the terminal cards and
// the orb that spirals in. The geometry is computed here, once; the browser
// only animates the ring and the orb.
func hero() template.HTML {
	const w, h, nx, ny = 1600, 700, 36, 16
	point := func(i, j int) (float64, float64) {
		u := float64(i)/nx*2 - 1 // across, -1 to 1
		v := float64(j) / ny     // depth, 0 at the horizon to 1 nearest
		du, dv := u*1.15, (v-0.46)*1.25
		r := math.Sqrt(du*du + dv*dv)
		dip := 150 / (1 + math.Pow(r/0.2, 2)) // the well
		x := 800 + u*(130+1350*math.Pow(v, 1.35))
		y := 300 + 420*math.Pow(v, 1.6) + dip*(0.35+0.65*v)
		return x, y
	}
	var b strings.Builder
	b.WriteString(`<div class="hero-art" aria-hidden="true"><div class="hero-sky"></div><div class="hero-stars"></div><div class="hero-sun"></div><div class="hero-ring"></div>`)
	fmt.Fprintf(&b, `<svg class="hero-mesh" viewBox="0 0 %d %d" preserveAspectRatio="xMidYMax slice" focusable="false">`, w, h)
	for j := 0; j <= ny; j++ {
		fmt.Fprintf(&b, `<path class="mx" style="--k:%.2f" d="`, float64(j)/ny)
		for i := 0; i <= nx; i++ {
			x, y := point(i, j)
			fmt.Fprintf(&b, "%s%.0f %.0f", map[bool]string{true: "M", false: "L"}[i == 0], x, y)
		}
		b.WriteString(`"/>`)
	}
	for i := 0; i <= nx; i++ {
		b.WriteString(`<path class="my" d="`)
		for j := 0; j <= ny; j++ {
			x, y := point(i, j)
			fmt.Fprintf(&b, "%s%.0f %.0f", map[bool]string{true: "M", false: "L"}[j == 0], x, y)
		}
		b.WriteString(`"/>`)
	}
	b.WriteString(`<circle class="orb" r="5"><animateMotion dur="11s" repeatCount="indefinite" path="M1560 520 C 1300 440, 1050 470, 960 520 C 880 565, 720 575, 700 540 C 690 515, 760 500, 800 520 C 830 535, 820 560, 800 560"/></circle></svg>`)
	b.WriteString(`<div class="hero-haze"></div><div class="hero-floor"></div>`)
	b.WriteString(`<div class="hero-term hero-term-a">$ wormhole ask bob <span class="c">"what do you think about X"</span><br><span class="m">thread</span> 01JB7Q open</div>`)
	b.WriteString(`<div class="hero-term hero-term-b">$ wormhole inbox<br><span class="m">[01JB7Q]</span> ^alice · what do you think…</div></div>`)
	return template.HTML(b.String())
}
