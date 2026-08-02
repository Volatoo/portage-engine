// Package webassets serves the built operator console — the Vite bundle under
// web/ — out of the Go binary, so the dashboard stays one self-contained
// artifact with nothing to deploy beside it.
//
// Two kinds of thing are served and they have opposite cache policies. A hashed
// asset carries a digest of its own bytes in its filename, so its URL can never
// mean two different files and the browser is told to keep it for a year.
// index.html is never cached: it is a template the server fills per request with
// the reader's resolved language and a boot payload, so the same URL
// legitimately produces different bytes for different readers.
package webassets

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
	"time"
)

// bundle/dist is written by `npm run build` (web/vite.config.ts points its
// outDir straight here, so there is no copy step that could carry a stale
// tree). It is not in git. bundle/.gitkeep is, and that file is the reason this
// directive still matches on a fresh clone: //go:embed fails the build when a
// pattern matches nothing, so without one committed file inside bundle/,
// `go build ./...` would need npm before it could compile at all.
//
//go:embed all:bundle
var bundle embed.FS

const (
	// distRoot is where Vite writes and where this package reads. The two are
	// kept in agreement by TestBuiltBundleIsMountedAtTheAssetPrefixTheServerServes,
	// which only has anything to check once a build has happened.
	distRoot = "bundle/dist"

	// indexFile is the shell. It is deliberately unreachable through the asset
	// handler: it is a template, and serving its unrendered bytes would ship
	// `{{.Boot}}` to a browser as text.
	indexFile = "index.html"

	// assetsDir is the one subtree Vite fills with content-hashed filenames, and
	// so the one subtree that may be served immutable. Anything Vite copies
	// verbatim lands outside it and is revalidated instead.
	assetsDir = "assets"

	immutableCache   = "public, max-age=31536000, immutable"
	revalidatedCache = "no-cache"
)

// ErrNotBuilt reports that this binary was compiled without a console bundle.
// It is a normal state for `go build ./...` on a working copy where npm has not
// run; it is never a normal state for a release binary, which is why the
// Makefile and the Dockerfile both build the bundle before the Go build.
var ErrNotBuilt = errors.New("webassets: console bundle not built (run `make web-build`)")

// Console is a loaded bundle: the shell as a template, and every other file
// with its bytes, its ETag and the cache policy its location earned.
type Console struct {
	files map[string]*asset
	index *template.Template
}

type asset struct {
	body        []byte
	etag        string
	contentType string
	immutable   bool
}

// Load reads the bundle compiled into this binary.
func Load() (*Console, error) {
	return loadFrom(bundle)
}

// loadFrom is Load against any tree, so the tests describe real behaviour on a
// fixture instead of describing whatever npm last produced on the machine.
func loadFrom(tree fs.FS) (*Console, error) {
	dist, err := fs.Sub(tree, distRoot)
	if err != nil {
		return nil, fmt.Errorf("webassets: %w", err)
	}
	// fs.Sub is happy to name a directory that is not there; the walk below is
	// where that shows up. An absent tree is the unbuilt case, not a broken one,
	// and it reports as such rather than as a stray "open .: file does not exist"
	// several layers from anything an operator can act on.
	if _, err := fs.Stat(dist, "."); err != nil {
		return nil, ErrNotBuilt
	}
	console := &Console{files: make(map[string]*asset)}
	walkErr := fs.WalkDir(dist, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		body, readErr := fs.ReadFile(dist, name)
		if readErr != nil {
			return fmt.Errorf("webassets: read %s: %w", name, readErr)
		}
		if name == indexFile {
			parsed, parseErr := template.New(indexFile).Parse(string(body))
			if parseErr != nil {
				return fmt.Errorf("webassets: parse %s: %w", name, parseErr)
			}
			console.index = parsed
			return nil
		}
		digest := sha256.Sum256(body)
		console.files[name] = &asset{
			body:        body,
			etag:        `"` + hex.EncodeToString(digest[:16]) + `"`,
			contentType: contentTypeFor(name),
			immutable:   strings.HasPrefix(name, assetsDir+"/"),
		}
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}
	if console.index == nil {
		return nil, ErrNotBuilt
	}
	return console, nil
}

// contentTypeFor names the type from the extension. mime's table is
// platform-dependent for a few of these — a Linux container without
// /etc/mime.types answers nothing for .js — so the types the bundle actually
// contains are stated here rather than inferred and hoped for.
func contentTypeFor(name string) string {
	switch path.Ext(name) {
	case ".js", ".mjs":
		return "text/javascript; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".json":
		return "application/json"
	case ".svg":
		return "image/svg+xml"
	case ".woff2":
		return "font/woff2"
	}
	if guessed := mime.TypeByExtension(path.Ext(name)); guessed != "" {
		return guessed
	}
	return "application/octet-stream"
}

// AssetHandler serves the hashed asset tree mounted at prefix. prefix must be
// the same string Vite was given as `base`, because that is what the built
// index.html wrote into every asset URL.
func (c *Console) AssetHandler(prefix string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		name := strings.TrimPrefix(r.URL.Path, prefix)
		// The lookup is an exact match against the list of files that were
		// compiled in, so traversal is not something a request can express: a
		// path outside the tree is simply a key that is not in the map. The
		// shell is excluded by name because it is a template, not a file.
		file, ok := c.files[name]
		if !ok || name == indexFile {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", file.contentType)
		w.Header().Set("ETag", file.etag)
		if file.immutable {
			w.Header().Set("Cache-Control", immutableCache)
		} else {
			w.Header().Set("Cache-Control", revalidatedCache)
		}
		// A zero modtime keeps Last-Modified out of the response: the bytes are
		// compiled in, so the only honest freshness statement is the ETag.
		http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(file.body))
	})
}

// Principal is who the server already knows the reader to be.
//
// A nil *Principal on Boot means "not resolved here", which is not the same as
// anonymous: the dashboard can name a local session's subject without leaving
// the process, but a federated principal lives in the control plane and costs a
// round trip, so it arrives later over /api/iam/me.
//
// This carries identity and never authority. No capability is asserted here, so
// a destination that needs one stays hidden until IAM answers — and an IAM
// outage leaves it hidden rather than revealing it and taking it back.
type Principal struct {
	Subject           string `json:"subject"`
	PreferredUsername string `json:"preferred_username"`
	ProviderID        string `json:"provider_id"`
	Authentication    string `json:"authentication"`
}

// Route is what the server resolved about the request from the URL alone, so a
// deep link renders its frame without a round trip for an id already in the
// address bar.
//
// Every field is always emitted. A route with no job id sends an empty string
// rather than omitting the key, so the bundle reads one shape and no code path
// branches on whether the server bothered to send a field.
type Route struct {
	// Name comes from the server's route table. It is not derived from the path
	// spelling, so a new route is an entry in that table rather than a string
	// comparison somewhere in the bundle.
	Name       string `json:"name"`
	Path       string `json:"path"`
	JobID      string `json:"job_id"`
	InstanceID string `json:"instance_id"`
	UserCode   string `json:"user_code"`
}

// IdentityProvider is one federated sign-in destination the deployment has
// actually initialised, named so the card can offer it by name.
//
// It carries what the deployment knows and not what the page knows: the id the
// start route is built from and the label to put on the button, but no URL. Where
// the reader lands afterwards and whether this trip is a step-up are facts about
// the page they are standing on, and the console builds a step-up link from three
// of them.
type IdentityProvider struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`

	// SupportsStepUp reports whether asking this provider to prove the reader
	// again means anything. handleProviderStart sends prompt=login and max_age=0
	// to an OIDC provider and cannot to a GitHub OAuth app, which has neither —
	// so a GitHub button on a step-up card returns a session exactly as old as
	// the one the control plane just refused.
	SupportsStepUp bool `json:"supports_step_up"`
}

// Boot is the payload the server stamps into the shell and the bundle reads
// synchronously before its first render.
//
// This struct is the source of truth for the field names; web/src/boot/payload.ts
// mirrors it. The names are snake_case because every other JSON surface the
// console reads is snake_case, and a boot payload spelled differently would be
// the one object in the app needing its own casing rule.
type Boot struct {
	// Lang is the language the server resolved: the cookie, then
	// Accept-Language, then English. The bundle agrees with it rather than
	// re-deciding, which is what keeps the first paint free of a flash and free
	// of the operating system's locale.
	Lang     string `json:"lang"`
	HTMLLang string `json:"html_lang"`

	AuthEnabled       bool `json:"auth_enabled"`
	OIDCEnabled       bool `json:"oidc_enabled"`
	LocalLoginEnabled bool `json:"local_login_enabled"`

	// IdentityProviders is every initialised provider, in configuration order, so
	// the sign-in card offers one destination per provider instead of one guess.
	// It is empty on a deployment configured the pre-multi-provider way, whose
	// single unnamed provider answers at /auth/oidc/start and nowhere else;
	// OIDCEnabled is what stays true there.
	IdentityProviders []IdentityProvider `json:"identity_providers"`

	Principal *Principal `json:"principal"`
	Route     Route      `json:"route"`

	// AssetBase is the prefix the hashed tree is mounted at, passed in rather
	// than compiled into the bundle so the two cannot drift apart.
	AssetBase string `json:"asset_base"`
}

// NoScript is the copy inside the shell's <noscript>, resolved per request the
// way HTMLLang beside it is.
//
// It is the one surface the bundle's catalogue cannot reach, and not by
// oversight: the browser this element renders in is the browser that will never
// run the bundle. The server has already resolved the reader's language for the
// attribute on <html>, so it is also the only thing in a position to choose
// these words.
//
// Each page name is the string internal/dashboard/ui.go already prints for that
// page — this element points the reader at that console, so it should point in
// that console's own words — and TestNoScriptNamesThePagesTheWayTheOldConsoleDoes
// compares the two. The dashboard package is what imports this one, so the
// values cannot be read from its catalogue directly; they are transcribed and
// then held against it, which is the arrangement the route tables and the status
// vocabulary are already in.
//
// The two sentences have no counterpart in that catalogue: a console that
// rendered on the server never had to tell anyone what scripting off costs them,
// so there was nothing to carry over and they shipped English in both languages.
// They are written here now, in the register of the bundle's own catalogue,
// because a Chinese reader whose browser runs no script is the one reader who
// cannot be sent anywhere else to be told why the page is blank. The same test
// pins that both languages have their own, so "the noscript is English only"
// cannot come back by deletion.
type NoScript struct {
	// NeedsScript names which failure this is. A reader who sees a page with no
	// content cannot tell a bundle that did not run from a server that is down,
	// and only one of those is worth reporting.
	NeedsScript string

	// StillServed introduces the destinations below it: the old console renders
	// on the server and answers every one of them without script.
	StillServed string

	// The five pages that console serves and this one links to, named rather
	// than spelled as paths, because a name is what a reader recognises and the
	// path is already in the href.
	Overview string
	Builds   string
	Packages string
	Docs     string
	Status   string
}

// NoScriptCopy resolves the <noscript> strings for a resolved language.
// Anything this console ships no strings for — including the empty string —
// answers in English, which is where resolveLanguage ends up for the same
// input.
//
// Exported because the check that keeps the Chinese honest lives in the
// dashboard package, which is where the catalogue it was taken from lives.
func NoScriptCopy(lang string) NoScript {
	text := NoScript{
		NeedsScript: "The operator console needs JavaScript. Nothing is wrong with the server " +
			"— this page is a bundle, and with scripting disabled there is nothing to run.",
		StillServed: "The previous console renders on the server and still answers:",
		Overview:    "Overview",
		Builds:      "Builds",
		Packages:    "Packages",
		Docs:        "Docs",
		Status:      "Status",
	}
	if lang != "zh" {
		return text
	}
	// The two sentences. 控制台 is what the whole product calls this thing
	// (brand.sub, device.console), 脚本包 says "bundle" without pretending the
	// reader knows the build tool's word for it, and 旧版控制台 names the server-
	// rendered console the links below point at. The em dash is doubled and
	// unspaced, which is how set.mirrors.hint already writes one.
	text.NeedsScript = "控制台需要 JavaScript。服务端没有问题——本页面就是一个脚本包," +
		"禁用脚本后没有任何东西可运行。"
	text.StillServed = "旧版控制台在服务端渲染,以下页面仍然可用:"
	// zhCatalogue in internal/dashboard/ui.go, keys nav.overview, nav.builds,
	// nav.packages, nav.docs and nav.status.
	text.Overview = "总览"
	text.Builds = "构建任务"
	text.Packages = "软件包"
	text.Docs = "文档"
	text.Status = "服务状态"
	return text
}

// indexData is what the shell template sees. HTMLLang, Lang and NoScript are
// lifted out of Boot because they are read before any script runs — two of them
// are attributes on <html>, and the third is for the reader who runs no script
// at all — so they have to be in the markup rather than in the payload.
type indexData struct {
	HTMLLang string
	Lang     string
	NoScript NoScript
	Boot     Boot
}

// RenderIndex writes the shell with boot stamped into it.
//
// The page is rendered into a buffer first. A template error halfway through a
// direct write would have already sent a 200 and half a document, which is the
// one failure mode a browser cannot tell from a truncated network read.
//
// {{.Boot}} sits in a script element, so html/template marshals it as JSON in a
// JavaScript context: `<`, `&` and the paragraph separators are escaped there,
// which is what makes a job id containing `</script>` a string rather than the
// end of the script.
func (c *Console) RenderIndex(w http.ResponseWriter, boot Boot) error {
	var page bytes.Buffer
	data := indexData{
		HTMLLang: boot.HTMLLang,
		Lang:     boot.Lang,
		NoScript: NoScriptCopy(boot.Lang),
		Boot:     boot,
	}
	if err := c.index.Execute(&page, data); err != nil {
		return fmt.Errorf("webassets: render shell: %w", err)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// The rendered language depends on the request, so a shared cache must not
	// hand a Chinese shell to the next English reader.
	w.Header().Add("Vary", "Cookie, Accept-Language")
	// The shell names hashed assets and carries the reader's own payload. It is
	// cheap to re-render and wrong to reuse.
	w.Header().Set("Cache-Control", "no-store")
	_, err := w.Write(page.Bytes())
	return err
}

// AssetPaths lists every file compiled in, sorted by nothing in particular. It
// exists for the test that asserts the built bundle and the server agree on
// where the tree is mounted.
func (c *Console) AssetPaths() []string {
	paths := make([]string, 0, len(c.files))
	for name := range c.files {
		paths = append(paths, name)
	}
	return paths
}

// IndexHTML renders the shell with a zero payload, for tests that need to read
// what the build produced rather than what a request would produce.
//
// The payload is zero; the copy is not. A shell rendered with an empty NoScript
// is a document with an empty <noscript> in it, which is a worse thing for a
// test to be reading than the English one every unresolved language gets.
func (c *Console) IndexHTML() (string, error) {
	var page bytes.Buffer
	if err := c.index.Execute(&page, indexData{NoScript: NoScriptCopy("")}); err != nil {
		return "", fmt.Errorf("webassets: render shell: %w", err)
	}
	return page.String(), nil
}
