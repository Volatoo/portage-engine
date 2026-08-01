package dashboard

import (
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/slchris/portage-engine/pkg/config"
)

// Each gate below pins one invariant of ui.go that has already been broken at
// least once, and was proved red first by reintroducing the defect in a scratch
// copy; the comment on each names the edit that makes it go red again.

// assembledPages is every page template the dashboard serves, keyed by the name
// it is registered under. A page added without being listed here escapes every
// gate in this file, which is why New() below cross-checks the count.
func assembledPages() map[string]string {
	return map[string]string{
		"landing": landingHTML, "login": loginHTML, "device": deviceAuthorizationHTML,
		"overview": overviewHTML, "builds": buildsPageHTML, "build-detail": buildDetailHTML,
		"logs": logsPageHTML, "monitor": monitorHTML, "image-factory": imageFactoryHTML,
		"settings": settingsHTML, "packages": packagesHTML, "docs": docsHTML,
		"status": statusHTML, "shell": shellHTML,
	}
}

var (
	i18nSlotPattern    = regexp.MustCompile(`data-i18n="([^"]+)"([^<>]*)>`)
	i18nDefaultPattern = regexp.MustCompile(`data-i18n-default="([^"]*)"`)
	anchorPattern      = regexp.MustCompile(`<a\s([^>]*)>`)
	placeholderPattern = regexp.MustCompile(`%[ds]`)
)

// Red by: adding data-i18n to an element whose text is empty, or one whose
// content contains a child element (localize refuses those, so no default is
// stamped and the toggle would blank the node).
func TestEveryMarkupKeyCarriesAnEnglishDefault(t *testing.T) {
	for page, html := range assembledPages() {
		for _, slot := range i18nSlotPattern.FindAllStringSubmatch(html, -1) {
			key, attrs := slot[1], slot[2]
			if strings.HasPrefix(key, "{{") {
				continue // the key itself is a template action; nothing to check
			}
			def := i18nDefaultPattern.FindStringSubmatch(attrs)
			if def == nil {
				t.Errorf("%s: data-i18n=%q has no data-i18n-default; "+
					"localize() did not recognise the slot", page, key)
				continue
			}
			if strings.TrimSpace(def[1]) == "" {
				t.Errorf("%s: data-i18n=%q has an empty English default", page, key)
			}
		}
	}
}

// The highest-yield i18n check: a renamed or dropped placeholder renders "%s"
// on screen or silently deletes a number from the sentence, and key parity
// sees neither.
// Red by: dropping the %s from zhCatalogue["login.oidc"], or the %d from any
// zhPlurals form.
func TestTranslationPlaceholdersMatchTheirEnglishDefault(t *testing.T) {
	english := map[string]string{}
	interpolated := map[string]bool{}
	for _, html := range assembledPages() {
		for _, slot := range i18nSlotPattern.FindAllStringSubmatch(html, -1) {
			if def := i18nDefaultPattern.FindStringSubmatch(slot[2]); def != nil {
				english[slot[1]] = def[1]
				// A slot carrying data-i18n-arg interpolates one value: the
				// English branch does it with the template action itself, the
				// translated branch with a %s the message layer fills in.
				interpolated[slot[1]] = strings.Contains(slot[2], "data-i18n-arg=")
			}
		}
	}
	for key, translated := range zhCatalogue {
		source, ok := english[key]
		if !ok {
			continue // JS-only string; its English lives at the call site
		}
		if interpolated[key] {
			if placeholders(translated) != "%s" {
				t.Errorf("%s: slot interpolates a value but zh=%q has no %%s", key, translated)
			}
			if !strings.Contains(source, "{{") {
				t.Errorf("%s: slot declares data-i18n-arg but the English default "+
					"%q interpolates nothing", key, source)
			}
			continue
		}
		if want, got := placeholders(source), placeholders(translated); want != got {
			t.Errorf("%s: English default has placeholders %q, zh has %q "+
				"(en=%q zh=%q)", key, want, got, source, translated)
		}
	}
	// Every plural form takes the count, in every category the catalogue lists.
	for key, forms := range zhPlurals {
		for category, form := range forms {
			if placeholders(form) != "%d" {
				t.Errorf("zhPlurals[%s][%s]=%q does not interpolate the count",
					key, category, form)
			}
		}
	}
}

func placeholders(s string) string {
	found := placeholderPattern.FindAllString(s, -1)
	sort.Strings(found)
	return strings.Join(found, ",")
}

// An <a> with no href is not a link: it takes no focus, so a click handler
// hung on it is unreachable by keyboard and invisible to a screen reader. The
// settings section links were exactly that — no href, no role, no tabindex —
// and a 120-Tab walk of the page found zero stops among them.
// Red by: removing href="#general" from the first subnav link.
func TestNoAnchorWithoutHref(t *testing.T) {
	for page, html := range assembledPages() {
		for _, anchor := range anchorPattern.FindAllStringSubmatch(html, -1) {
			attrs := anchor[1]
			if strings.Contains(attrs, "href=") {
				continue
			}
			t.Errorf("%s: <a %s> has no href; it cannot be reached or "+
				"operated from the keyboard", page, strings.TrimSpace(attrs))
		}
	}
}

// document.querySelectorAll('[data-system-admin]') matched <body> itself,
// which carried the same attribute as a state flag, and .remove() on the first
// hit deleted the page. Two things keep that from coming back: the query is
// scoped to document.body, and the body-level state has a different name from
// the per-control gate.
// Red by: changing document.body.querySelectorAll back to document.querySelectorAll
// in resolveCapabilities, or by setting data-capability on document.body.
func TestCapabilityQueryIsScopedToTheBody(t *testing.T) {
	if strings.Contains(baseJS, "document.querySelectorAll('[data-capability]')") {
		t.Error("baseJS queries [data-capability] on the document; " +
			"an element carrying that attribute above the body would be removed with the page")
	}
	if !strings.Contains(baseJS, "document.body.querySelectorAll('[data-capability]')") {
		t.Error("baseJS no longer scopes the capability query to document.body")
	}
	if strings.Contains(baseJS, "body.setAttribute('data-capability'") {
		t.Error("the body-level principal state must not reuse the per-control gate attribute")
	}
	if !strings.Contains(baseJS, "setAttribute('data-principal-scope'") {
		t.Error("the body-level principal state attribute is gone")
	}
}

// One declaration decides who sees a route. The removal pass used to re-spell
// three route paths as JS selectors while the Go nav table carried the same
// routes with no capability field at all, so a route added to the table
// defaulted to visible. Both halves read the declaration now, and a gated
// destination ships hidden so an IAM call that fails leaves it hidden.
// Red by: dropping capabilitySystemAdmin from the /settings entry in consoleNav,
// or removing the hidden attribute from the rendered gated anchor.
func TestGatedNavRoutesShipHiddenAndDeclareTheirCapability(t *testing.T) {
	gated := map[string]bool{"/monitor": true, "/image-factory": true, "/settings": true}
	for _, entry := range consoleNav {
		if gated[entry.path] && entry.capability != capabilitySystemAdmin {
			t.Errorf("consoleNav %s declares capability %q, want %q",
				entry.path, entry.capability, capabilitySystemAdmin)
		}
		if !gated[entry.path] && entry.capability != "" {
			t.Errorf("consoleNav %s unexpectedly declares capability %q",
				entry.path, entry.capability)
		}
	}
	for _, path := range []string{"/monitor", "/image-factory", "/settings"} {
		anchor := `<a class="nav-item" href="` + path + `"`
		index := strings.Index(overviewHTML, anchor)
		if index < 0 {
			t.Fatalf("overview page renders no nav anchor for %s", path)
		}
		tag := overviewHTML[index : strings.Index(overviewHTML[index:], ">")+index]
		if !strings.Contains(tag, `data-capability="`+capabilitySystemAdmin+`"`) {
			t.Errorf("%s: rendered anchor carries no capability: %s", path, tag)
		}
		if !strings.Contains(tag, " hidden") {
			t.Errorf("%s: rendered anchor is not hidden, so an IAM failure "+
				"leaves it reachable: %s", path, tag)
		}
	}
}

// One backend vocabulary, one table. The colour map and the label catalogue
// were two hand-maintained mirrors of it and had already diverged — busy and
// draining had a colour and no Chinese label, so a Chinese page rendered the
// raw English token next to a correctly coloured dot.
// Red by: deleting the busy or draining entry, or giving one an empty label.
func TestStatusVocabularyIsOneTable(t *testing.T) {
	colours := map[string]bool{"gray": true, "orange": true, "blue": true, "green": true, "red": true}
	for token, entry := range statusVocabulary {
		if !colours[entry.Color] {
			t.Errorf("status %q has colour %q, which no .status rule declares", token, entry.Color)
		}
		if strings.TrimSpace(entry.ZH) == "" {
			t.Errorf("status %q has a colour but no Chinese label", token)
		}
	}
	// The two the backend emits that the split tables had lost.
	for _, token := range []string{"busy", "draining"} {
		if _, ok := statusVocabulary[token]; !ok {
			t.Errorf("statusVocabulary has no entry for the builder status %q", token)
		}
	}
	if strings.Contains(baseJS, "STATUS_COLORS") {
		t.Error("baseJS still carries a second status table")
	}
}

// The language is resolved before the first byte, so the response carries the
// right <html lang> and the right strings. Nothing swaps a frame later, which
// is what a client-side rewrite does: a fully laid-out English page on screen,
// then every string replaced under the reader.
// Red by: putting <html lang="en"> back on any shell, or by dropping the
// localize() call from appPage/publicPage.
func TestPagesRenderInTheResolvedLanguage(t *testing.T) {
	dashboard := New(&config.DashboardConfig{})
	for _, tc := range []struct{ route, en, zh string }{
		{"/overview", "Overview", "总览"},
		{"/settings", "Settings", "设置"},
		{"/monitor", "Build Nodes", "构建节点"},
		{"/packages", "Packages", "软件包"},
		{"/status", "Service Status", "服务状态"},
		{"/", "Portage Engine — self-hosted", "自托管"},
	} {
		for _, lang := range []struct{ header, tag, want string }{
			{"en", "en", tc.en},
			{"zh-CN", "zh-CN", tc.zh},
		} {
			request := httptest.NewRequest("GET", tc.route, nil)
			request.Header.Set("Accept-Language", lang.header)
			recorder := httptest.NewRecorder()
			dashboard.Router().ServeHTTP(recorder, request)
			body := recorder.Body.String()
			title := betweenTags(body, "<title", "</title>")
			if !strings.Contains(body, `<html lang="`+lang.tag+`"`) {
				t.Errorf("%s (%s): response does not carry <html lang=%q>",
					tc.route, lang.header, lang.tag)
			}
			if !strings.Contains(title, lang.want) {
				t.Errorf("%s (%s): first-paint title is %q, want it to contain %q",
					tc.route, lang.header, title, lang.want)
			}
		}
	}
}

// The stylesheet is served with Cache-Control: immutable, which tells a browser
// never to revalidate it: the URL is the only thing that can retire a cached
// copy, and the ETag will never be consulted to do it. Six hand-written ?v=
// literals had to be bumped together, and the first edit that missed one would
// have shipped a year-stale stylesheet to every returning reader.
// Red by: writing a literal (?v=2) back into any page's stylesheet link.
func TestStylesheetLinksCarryTheContentDigest(t *testing.T) {
	link := regexp.MustCompile(`href="/static/apple\.css\?v=([^"]*)"`)
	for page, html := range assembledPages() {
		found := link.FindAllStringSubmatch(html, -1)
		if len(found) == 0 {
			t.Errorf("%s: links no stylesheet", page)
		}
		for _, match := range found {
			if match[1] != appleCSSDigest {
				t.Errorf("%s: stylesheet link carries %q, not the stylesheet's own "+
					"content digest %q; an immutable response behind a stale URL "+
					"is never revalidated", page, match[1], appleCSSDigest)
			}
		}
	}
}

func betweenTags(body, open, close string) string {
	start := strings.Index(body, open)
	if start < 0 {
		return ""
	}
	start += strings.Index(body[start:], ">") + 1
	end := strings.Index(body[start:], close)
	if end < 0 {
		return ""
	}
	return body[start : start+end]
}

// Every page the dashboard registers must be listed in assembledPages(), or the
// gates above silently stop covering it.
// Red by: registering a new template in New() without listing it here.
func TestAssembledPagesCoversEveryRegisteredTemplate(t *testing.T) {
	dashboard := New(&config.DashboardConfig{})
	pages := assembledPages()
	for _, tmpl := range dashboard.templates.Templates() {
		name := tmpl.Name()
		if name == "" {
			continue
		}
		if _, ok := pages[name]; !ok {
			t.Errorf("template %q is registered but not listed in assembledPages()", name)
		}
	}
}
