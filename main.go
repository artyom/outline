package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"unicode"

	"rsc.io/markdown"
)

var exeName = filepath.Base(os.Args[0])

func main() {
	log.SetFlags(0)
	commands := []subcommand{
		{name: "get", fn: handleGet, desc: "download a single document"},
		{name: "update", fn: handleUpdate, desc: "replace document with a content from file"},
	}
	usage := func() {
		w := flag.CommandLine.Output()
		fmt.Fprintf(w, "Usage: %s [subcommand] [flags]\n", exeName)
		for _, c := range commands {
			fmt.Fprintf(w, "\t%-15s %s\n", c.name, c.desc)
		}
		os.Exit(2)
	}
	if len(os.Args) < 2 {
		usage()
	}
	for _, cmd := range commands {
		if os.Args[1] != cmd.name {
			continue
		}
		token := authToken(os.Getenv("OUTLINE_TOKEN"))
		if token == "" {
			log.Fatal("OUTLINE_TOKEN is not set")
		}
		if err := cmd.fn(context.Background(), token, os.Args[2:]); err != nil {
			log.Fatal(err)
		}
		return
	}
	usage()
}

type subcommand struct {
	name string
	desc string
	fn   func(context.Context, authToken, []string) error
}

func handleUpdate(ctx context.Context, token authToken, cliargs []string) error {
	var urlid string
	fs := flag.NewFlagSet("", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: %s update [flags] source-document.md\n", exeName)
		fs.PrintDefaults()
	}
	fs.StringVar(&urlid, "id", urlid, "document url|urlid")
	fs.Parse(cliargs)
	if fs.NArg() == 0 {
		return errors.New("want source document as the first positional argument")
	}
	if urlid == "" {
		return errors.New("-id flag must be set")
	}
	if i := strings.LastIndexByte(urlid, '-'); i != -1 {
		urlid = urlid[i+1:]
	}
	data, err := os.ReadFile(fs.Arg(0))
	if err != nil {
		return err
	}
	var p markdown.Parser
	doc := p.Parse(string(data))
	title := docTitle(doc)
	dropLeadingH1(doc)
	rewriteHeadingLinks(doc)
	req := struct {
		Id    string `json:"id"`
		Title string `json:"title,omitempty"`
		Text  string `json:"text"`
	}{
		Id:    urlid,
		Title: title,
		Text:  markdown.Format(doc),
	}
	var res struct{}
	return doApiRequest(ctx, req, &res, token, "https://app.getoutline.com/api/documents.update")
}

func handleGet(ctx context.Context, token authToken, cliargs []string) error {
	var dstFile string
	fs := flag.NewFlagSet("", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: %s get [flags] url|urlid\n", exeName)
		fs.PrintDefaults()
	}
	fs.StringVar(&dstFile, "o", dstFile, "file to save result to, if not set, it will be printed to stdout")
	fs.Parse(cliargs)
	if fs.NArg() == 0 {
		return errors.New("want document url/urlid as the first positional argument")
	}
	urlid := fs.Arg(0)
	if i := strings.LastIndexByte(urlid, '-'); i != -1 {
		urlid = urlid[i+1:]
	}
	req := struct {
		Id string `json:"id"`
	}{Id: urlid}
	var res struct {
		Data struct {
			Title string `json:"title"`
			Text  string `json:"text"`
		} `json:"data"`
	}
	if err := doApiRequest(ctx, req, &res, token, "https://app.getoutline.com/api/documents.info"); err != nil {
		return err
	}
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "# %s\n\n", res.Data.Title)
	fmt.Fprintln(&buf, res.Data.Text)
	if dstFile != "" && dstFile != "-" {
		return os.WriteFile(dstFile, buf.Bytes(), 0666)
	}
	_, err := os.Stdout.Write(buf.Bytes())
	return err
}

func doApiRequest(ctx context.Context, reqObject, respObjectPtr any, token authToken, endpoint string) error {
	if reflect.ValueOf(respObjectPtr).Kind() != reflect.Pointer {
		panic("doApiRequest expects respObjectPtr to be a pointer")
	}
	body, err := json.Marshal(reqObject)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", token.bearer())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusBadRequest {
			var msg json.RawMessage
			if json.NewDecoder(resp.Body).Decode(&msg) == nil {
				return &badRequestError{data: string(msg)}
			}
		}
		return fmt.Errorf("unexpected status: %s", resp.Status)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		return fmt.Errorf("unexpected content-type: %s", ct)
	}
	dec := json.NewDecoder(resp.Body)
	return dec.Decode(respObjectPtr)
}

type authToken string

func (t authToken) bearer() string { return "Bearer " + string(t) }

type badRequestError struct {
	data string
}

func (e *badRequestError) Error() string {
	if e.data != "" {
		return "Bad request: " + e.data
	}
	return "Bad request"
}

func docTitle(doc *markdown.Document) string {
	for _, b := range doc.Blocks {
		h, ok := b.(*markdown.Heading)
		if !ok {
			continue
		}
		return inlinesText(h.Text.Inline)
	}
	return ""
}

func inlinesText(inl markdown.Inlines) string {
	var b strings.Builder
	for _, e := range inl {
		switch x := e.(type) {
		case *markdown.Plain:
			b.WriteString(x.Text)
		case *markdown.Escaped:
			b.WriteString(x.Text)
		case *markdown.Emoji:
			b.WriteString(x.Text)
		case *markdown.Strong:
			b.WriteString(inlinesText(x.Inner))
		case *markdown.Emph:
			b.WriteString(inlinesText(x.Inner))
		}
	}
	return b.String()
}

func dropLeadingH1(doc *markdown.Document) {
	if len(doc.Blocks) == 0 {
		return
	}
	if h, ok := doc.Blocks[0].(*markdown.Heading); ok && h.Level == 1 {
		doc.Blocks = doc.Blocks[1:]
	}
}

// rewriteHeadingLinks rewrites links to document subsections (headers) from
// github|vscode-compatible to Outline-compatible style.
func rewriteHeadingLinks(doc *markdown.Document) {
	slugs := make(map[string]string) // regular slug to outline-style slug
	for _, b := range doc.Blocks {
		h, ok := b.(*markdown.Heading)
		if !ok {
			continue
		}
		text := inlinesText(h.Text.Inline)
		slugs["#"+slugRegular(text)] = "#" + slugOutline(text)
	}
	if len(slugs) == 0 {
		return
	}
	var updateInlines func(markdown.Inlines)
	updateInlines = func(inlines markdown.Inlines) {
		for _, inl := range inlines {
			switch ent := inl.(type) {
			case *markdown.Strong:
				updateInlines(ent.Inner)
			case *markdown.Emph:
				updateInlines(ent.Inner)
			case *markdown.Link:
				if u, ok := slugs[ent.URL]; ok {
					ent.URL = u
				}
			}
		}
	}

	var walkBlocks func(markdown.Block)
	walkBlocks = func(block markdown.Block) {
		switch bl := block.(type) {
		case *markdown.Item:
			for _, b := range bl.Blocks {
				walkBlocks(b)
			}
		case *markdown.List:
			for _, b := range bl.Items {
				walkBlocks(b)
			}
		case *markdown.Paragraph:
			updateInlines(bl.Text.Inline)
		case *markdown.Quote:
			for _, b := range bl.Blocks {
				walkBlocks(b)
			}
		case *markdown.Text:
			updateInlines(bl.Inline)
		}
	}
	for _, b := range doc.Blocks {
		walkBlocks(b)
	}
}

// slugRegular generates header id slug in a way similar to how github or vscode do it
func slugRegular(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return unicode.ToLower(r)
		}
		return '-'
	}, s)
}

// slugOutline generates header id slug in a way similar to how Outline does it
func slugOutline(s string) string {
	// https://github.com/outline/outline/blob/28cc83ad05764278fe4fad57645e8de7c6430274/shared/editor/lib/headingToSlug.ts#L10-L24
	var prevDash bool
	fn := func(r rune) rune {
		for _, r2 := range "[!\"#$%&'.()*+,\\/:;<=>?@[]\\^_`{|}~]" {
			if r == r2 {
				return -1
			}
		}
		if unicode.IsSpace(r) || unicode.IsPunct(r) {
			if !prevDash {
				prevDash = true
				return '-'
			}
			return -1
		}
		prevDash = false
		return unicode.ToLower(r)
	}
	return "h-" + strings.TrimRight(strings.Map(fn, s), "-")
}
