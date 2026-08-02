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

// The build detail page carried two elements with the same class: an empty
// pre for the job error and the live log pane. Anything reading .log-view on
// that page — a reader hunting for the log, a probe measuring it — got the
// error box, which is empty on a job that succeeded and holds one sentence on
// a job that failed, and concluded the log renderer was dead.
// Red by: dressing the failure report as a <pre class="log-view"> again.
func TestBuildDetailHasExactlyOneLogPane(t *testing.T) {
	if n := strings.Count(buildDetailHTML, `class="log-view"`); n != 1 {
		t.Errorf("build detail carries %d .log-view elements; the log pane must be "+
			"the only thing on the page wearing the class that names it", n)
	}
	if !strings.Contains(buildDetailHTML, `<pre class="log-view" id="live-log">`) {
		t.Error("the one .log-view on build detail is not the live log pane")
	}
}

// What an operator opens a failed build to read comes first. The error used to
// be the last card on the page: below the fold at 1280 and at 375, under eight
// surfaces that had nothing to do with the failure.
// Red by: moving the failure section back below the pipeline or the log.
func TestBuildDetailLeadsWithTheFailureReport(t *testing.T) {
	order := []string{`id="head"`, `id="failure"`, `id="pipeline"`, `id="meta"`, `id="live-log"`}
	previous := -1
	for _, anchor := range order {
		at := strings.Index(buildDetailHTML, anchor)
		if at < 0 {
			t.Fatalf("build detail no longer renders %s", anchor)
		}
		if at < previous {
			t.Errorf("%s is rendered above the surface that must precede it", anchor)
		}
		previous = at
	}
}

// Every state the stage machine can produce needs a word, because the word is
// the channel a reader who cannot use the dot's hue is left with. A state with
// no entry prints its own raw token beside the stage name.
// Red by: returning a new state from stageState without adding it to
// STAGE_STATE_WORDS.
func TestEveryPipelineStageStateHasAWord(t *testing.T) {
	words := map[string]bool{}
	table := regexp.MustCompile(`(\w+):\s+\{ key: 'stage\.(\w+)'`)
	for _, entry := range table.FindAllStringSubmatch(buildDetailJS, -1) {
		if entry[1] != entry[2] {
			t.Errorf("STAGE_STATE_WORDS[%s] is keyed on stage.%s; the state and its "+
				"message key must be the same token", entry[1], entry[2])
		}
		words[entry[1]] = true
	}
	if len(words) == 0 {
		t.Fatal("STAGE_STATE_WORDS is gone; the stage states have no words at all")
	}
	body := jsFunctionBody(t, buildDetailJS, "function stageState(")
	quoted := regexp.MustCompile(`'(\w+)'`)
	returned := regexp.MustCompile(`return ([^;]*);`)
	statements := returned.FindAllStringSubmatch(body, -1)
	if len(statements) == 0 {
		t.Fatal("stageState returns nothing this gate can read")
	}
	for _, statement := range statements {
		for _, state := range quoted.FindAllStringSubmatch(statement[1], -1) {
			if !words[state[1]] {
				t.Errorf("stageState returns %q, which STAGE_STATE_WORDS has no word for", state[1])
			}
		}
	}
}

// A remedy that points at a settings panel which is not there is worse than no
// remedy: it answers "where do I fix this" with a page that does not scroll to
// anything. Both halves are checked — the panel the fragment resolves to, and
// the field label the sentence names.
// Red by: renaming a settings panel, or a field's message key, without moving
// the entry in ERROR_REMEDIES with it.
func TestErrorRemediesResolveToRealSettings(t *testing.T) {
	panels := regexp.MustCompile(`panel: '([\w-]+)'`).FindAllStringSubmatch(buildDetailJS, -1)
	labels := regexp.MustCompile(`(?:field|section): '([\w.]+)'`).FindAllStringSubmatch(buildDetailJS, -1)
	if len(panels) == 0 || len(labels) != 2*len(panels) {
		t.Fatalf("ERROR_REMEDIES has %d panels and %d labels; every console-owned "+
			"entry names a field and the section holding it", len(panels), len(labels))
	}
	for _, panel := range panels {
		if !strings.Contains(settingsHTML, `data-panel="`+panel[1]+`"`) ||
			!strings.Contains(settingsHTML, `id="`+panel[1]+`"`) {
			t.Errorf("a remedy links to /settings#%s, which resolves to no panel", panel[1])
		}
	}
	for _, label := range labels {
		if !strings.Contains(settingsHTML, `data-i18n="`+label[1]+`"`) {
			t.Errorf("a remedy names %q, which labels nothing on the settings page", label[1])
		}
	}
	// The other shape: a value this console does not own is named and never
	// linked, because there is no field for a link to land on.
	for _, env := range regexp.MustCompile(`env: '([A-Z0-9_]+)'`).FindAllStringSubmatch(buildDetailJS, -1) {
		if strings.Contains(settingsHTML, strings.ToLower(env[1])) {
			t.Errorf("%s is named as a deployment key but the settings page carries a "+
				"control for it; link the reader to the control instead", env[1])
		}
	}
}

// pageScripts returns the <script> body every assembled page ships, keyed by
// the page. The three gates below read the shipped script rather than the const
// that feeds it: a formatter is defined in one const and called from six
// others, and only the assembled page proves the two arrived together.
func pageScripts() map[string]string {
	scripts := map[string]string{}
	block := regexp.MustCompile(`(?s)<script>(.*?)</script>`)
	for page, html := range assembledPages() {
		if found := block.FindStringSubmatch(html); found != nil {
			scripts[page] = found[1]
		}
	}
	return scripts
}

// enclosingJSFunction names the function a byte offset sits inside, by reading
// back to the nearest declaration. The scripts declare no nested functions
// above the sites this is used on, so the nearest declaration is the owner.
var jsDeclaration = regexp.MustCompile(`function (\w+)\(`)

func enclosingJSFunction(prefix string) string {
	found := jsDeclaration.FindAllStringSubmatch(prefix, -1)
	if len(found) == 0 {
		return "(top level)"
	}
	return found[len(found)-1][1]
}

// Go's zero time.Time is on the wire — a time.Time field serialises as
// 0001-01-01T00:00:00Z whether or not the event it names has happened, and
// omitempty cannot suppress it because a struct is never empty — so a locale
// formatter handed one returned "1/1/1, 8:05:43 AM". The Job Ledger printed
// that beside its write-error count, which are the two numbers a reader uses to
// decide whether the ledger can be trusted at all. Two formatters own every
// clock reading on the console and the public pages, and both resolve the value
// through the one predicate first.
// Red by: writing new Date(x).toLocaleString(...) at any call site, or dropping
// the peInstant call from either formatter.
func TestEveryClockReadingResolvesThroughTheInstantGuard(t *testing.T) {
	formatters := map[string]bool{"fmtTime": true, "fmtPublicTime": true}
	locale := regexp.MustCompile(`toLocale(?:String|DateString|TimeString)\(`)
	seen := 0
	for page, script := range pageScripts() {
		for _, at := range locale.FindAllStringIndex(script, -1) {
			seen++
			if owner := enclosingJSFunction(script[:at[0]]); !formatters[owner] {
				t.Errorf("%s: %s() formats a clock reading itself; every timestamp goes "+
					"through the two formatters that resolve peInstant first, or a Go "+
					"zero time renders as a real date", page, owner)
			}
		}
	}
	if seen == 0 {
		t.Fatal("no page formats a timestamp at all; this gate no longer reads the code")
	}
	// The predicate itself, and everything that subtracts two instants rather
	// than printing one — a duration is the site where an unresolvable value
	// does not announce itself, because it comes back as a number.
	if body := jsFunctionBody(t, i18nRuntimeJS, "function peInstant("); !strings.Contains(body, "getUTCFullYear") {
		t.Error("peInstant no longer holds a year floor, so it accepts the zero time as an instant")
	}
	for _, owner := range []struct{ source, signature string }{
		{baseRuntimeJS, "function fmtTime("},
		{publicJS, "function fmtPublicTime("},
		{baseRuntimeJS, "function fmtTimeRange("},
		{buildDetailJS, "function durationTile("},
	} {
		if !strings.Contains(jsFunctionBody(t, owner.source, owner.signature), "peInstant(") {
			t.Errorf("%s does not resolve its value through peInstant", owner.signature)
		}
	}
	// new Date() with no argument is the clock, not a parse; every other
	// construction belongs to the guard.
	construction := regexp.MustCompile(`new Date\((.)`)
	for page, script := range pageScripts() {
		for _, at := range construction.FindAllStringSubmatchIndex(script, -1) {
			if script[at[2]:at[3]] == ")" {
				continue
			}
			if owner := enclosingJSFunction(script[:at[0]]); owner != "peInstant" {
				t.Errorf("%s: %s() parses a timestamp outside peInstant", page, owner)
			}
		}
	}
}

// The Job Ledger card reported "consistent" while showing "write errors 19" and
// a last-checked date that was the zero time wearing a locale format. Three
// judgements of one fact: the badge read ledger.ok, the count was rendered
// beside it in the meta row's own ink, and the reconcile timestamp was gated on
// a truthiness test the zero value passes. One token now decides all three, and
// it is a token from the shared vocabulary rather than a colour picked here.
// Red by: badging on ledger.ok alone again, or dropping the never word from the
// checked_at line so the zero timestamp is formatted.
func TestJobLedgerVerdictReadsItsOwnNumbers(t *testing.T) {
	if strings.Contains(monitorJS, "statusBadge(ledger.ok") {
		t.Error("the ledger badge is computed from ledger.ok alone, so a non-zero " +
			"write-error count can sit two lines under the word \"consistent\"")
	}
	verdict := regexp.MustCompile(`(?s)var ledgerToken = (.*?);`).FindStringSubmatch(monitorJS)
	if verdict == nil {
		t.Fatal("the ledger card no longer computes one token for its whole verdict")
	}
	for _, term := range []string{"writeErrors", "reconciledAt"} {
		if !strings.Contains(verdict[1], term) {
			t.Errorf("the ledger verdict does not account for %s", term)
		}
	}
	if !strings.Contains(monitorJS, "fmtTime(reconcile.checked_at, t('common.never'") {
		t.Error("the last-checked line no longer names the never state, so a reconcile " +
			"that has not run renders as a date")
	}
	if strings.Contains(monitorJS, "if (reconcile.checked_at)") {
		t.Error("the last-checked line is gated on a truthiness test that the zero " +
			"timestamp passes and the absent field fails — the reverse of what it needs")
	}
}

// Conclusion-bearing text takes the deepened ink the token block declares for
// it, and takes it from the same table the badge beside it reads. Every status
// token spelled in the monitor's verdict expressions has to be one that table
// carries, or the ink falls to the catch-all and a breach renders as quietly as
// the cost figure under it.
// Red by: inventing a token in a *Token expression, or binding .verdict to a
// colour instead of --successInk / --dangerInk.
func TestConclusionInkComesFromTheStatusVocabulary(t *testing.T) {
	if !strings.Contains(jsFunctionBody(t, baseRuntimeJS, "function verdictInk("), "STATUS_VOCABULARY[") {
		t.Fatal("verdictInk no longer reads the status vocabulary")
	}
	assignments := regexp.MustCompile(`(?s)var \w*Token = (.*?);`).FindAllStringSubmatch(monitorJS, -1)
	if len(assignments) == 0 {
		t.Fatal("the monitor spells no status token; this gate no longer reads the code")
	}
	quoted := regexp.MustCompile(`'([a-z_]+)'`)
	for _, assignment := range assignments {
		for _, token := range quoted.FindAllStringSubmatch(assignment[1], -1) {
			if _, ok := statusVocabulary[token[1]]; !ok {
				t.Errorf("a verdict is keyed on %q, which statusVocabulary does not carry; "+
					"the ink falls to the catch-all and the conclusion reads as an aside", token[1])
			}
		}
	}
	// The other half of the pair: nothing outside the table may pick a status
	// colour, and the two verdict inks are the tokens the ladder declares.
	for _, forbidden := range []string{"successInk", "dangerInk", "'status green'", "'status red'"} {
		if strings.Contains(monitorJS, forbidden) {
			t.Errorf("monitorJS spells %s itself instead of reading the status table", forbidden)
		}
	}
	for ink, verdict := range map[string]string{"--successInk": "green", "--dangerInk": "red"} {
		rule := `.verdict[data-verdict="` + verdict + `"]`
		if !strings.Contains(appleCSS, rule) || !strings.Contains(appleCSS, "var("+ink+")") {
			t.Errorf("%s does not bind var(%s)", rule, ink)
		}
	}
}

// Five states, one renderer. Every route re-derived loading / empty /
// filtered-empty / error / partial for itself and each lost a different one: a
// filtered result showed the copy that invites you to publish your first
// package, and a failed load rendered into the same node as an account with
// nothing in it. pageState is now the only thing that builds a .empty node, and
// it is what puts the live region on the box.
// Red by: appending el('div', 'empty', …) at any call site again.
func TestEveryStateBoxComesFromOneRenderer(t *testing.T) {
	if n := strings.Count(i18nRuntimeJS, "className = 'empty'"); n != 1 {
		t.Fatalf("pageState is not the only builder of a state node (%d found)", n)
	}
	handBuilt := regexp.MustCompile(`(?:el|createElement)\(\s*'div',\s*'empty'`)
	for page, script := range pageScripts() {
		if handBuilt.MatchString(script) {
			t.Errorf("%s builds a state node outside pageState, so it carries neither "+
				"the state attribute nor the live region", page)
		}
	}
	body := jsFunctionBody(t, i18nRuntimeJS, "function pageState(")
	for _, required := range []string{"'status'", "aria-live", "data-state"} {
		if !strings.Contains(body, required) {
			t.Errorf("pageState no longer writes %s, so a reader is not told the content changed", required)
		}
	}
	// The state a search produces is not the state an unpublished binhost
	// produces, and the two copies must not be the same string.
	if zhCatalogue["packages.none"] == zhCatalogue["packages.empty"] {
		t.Error("the filtered-empty and empty copies are identical; a first-time reader " +
			"is told to change a search they never made")
	}
}

// jsFunctionBody returns the brace-delimited body of the named JS function.
func jsFunctionBody(t *testing.T, source, signature string) string {
	t.Helper()
	start := strings.Index(source, signature)
	if start < 0 {
		t.Fatalf("%q is not in the script", signature)
	}
	open := strings.Index(source[start:], "{") + start
	depth := 0
	for i := open; i < len(source); i++ {
		switch source[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return source[open+1 : i]
			}
		}
	}
	t.Fatalf("%q is never closed", signature)
	return ""
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
