package dashboard

// css_invariants_test.go gates the stylesheet's own rules. Every class of
// defect checked here shipped at least once and none of them could go red:
// three tokens were referenced but never declared, four dark-mode tokens were
// missing so elevation silently stopped working, seven radii were literals,
// and the whole type ramp was in px. A parser is the cheapest thing that
// refuses the next instance rather than the last one.

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// cssDecl is one `prop: value` pair, carrying the selector and at-rule chain it
// was found under so an allowlist can be scoped to a place, not just a value.
type cssDecl struct {
	atRules  []string
	selector string
	prop     string
	value    string
}

// parseCSS walks appleCSS and returns every declaration with its context. It
// handles nesting (@supports > @media > :root) but assumes declarations never
// appear directly inside an at-rule, which is true of that stylesheet. The
// inline <style> in shellHTML is deliberately out of scope: it is parsed after
// appleCSS and wins on equal specificity, so it needs its own gate rather than
// being folded into these invariants.
func parseCSS() []cssDecl {
	css := regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(appleCSS, "")

	var (
		out      []cssDecl
		atRules  []string
		selector string
		prelude  strings.Builder
	)
	// Selector stack depth: a non-empty selector means we are inside a rule
	// body and everything up to `}` is a declaration.
	inRule := false
	for i := 0; i < len(css); i++ {
		switch c := css[i]; c {
		case '{':
			text := strings.TrimSpace(prelude.String())
			prelude.Reset()
			if strings.HasPrefix(text, "@") {
				atRules = append(atRules, text)
				continue
			}
			selector, inRule = text, true
		case '}':
			prelude.Reset()
			if inRule {
				inRule, selector = false, ""
				continue
			}
			if len(atRules) > 0 {
				atRules = atRules[:len(atRules)-1]
			}
		case ';':
			text := strings.TrimSpace(prelude.String())
			prelude.Reset()
			if !inRule {
				continue
			}
			prop, value, ok := strings.Cut(text, ":")
			if !ok {
				continue
			}
			out = append(out, cssDecl{
				atRules:  append([]string(nil), atRules...),
				selector: selector,
				prop:     strings.TrimSpace(prop),
				value:    strings.TrimSpace(value),
			})
		default:
			prelude.WriteByte(c)
		}
	}
	return out
}

// TestCSSTokensAreDeclared fails on a var(--x) whose name is declared nowhere.
// An undeclared token does not fall back to anything sensible: the property is
// dropped, so `border: 1px solid var(--separator)` computed to no border at all
// and `font: var(--caption-1)` left the label at inherited body size.
func TestCSSTokensAreDeclared(t *testing.T) {
	decls := parseCSS()

	declared := map[string]bool{}
	for _, d := range decls {
		if strings.HasPrefix(d.prop, "--") {
			declared[d.prop] = true
		}
	}

	ref := regexp.MustCompile(`var\(\s*(--[A-Za-z0-9_-]+)`)
	for _, d := range decls {
		for _, m := range ref.FindAllStringSubmatch(d.value, -1) {
			if !declared[m[1]] {
				t.Errorf("%s { %s } references %s, which is declared nowhere in appleCSS", d.selector, d.prop, m[1])
			}
		}
	}
}

// colorValue reports whether a token's value carries a colour of its own, in
// any of the forms this stylesheet uses. Values built only out of other tokens
// (--keyline, --controlBG) are mode-correct by construction and are not colours
// for this purpose.
var colorValue = regexp.MustCompile(`#[0-9a-fA-F]{3,8}\b|rgba?\(\s*[0-9]|hsla?\(\s*[0-9]|^\s*[0-9]{1,3}\s*,\s*[0-9]{1,3}\s*,\s*[0-9]{1,3}\s*$`)

const darkQuery = "@media (prefers-color-scheme: dark)"

// rootTokens returns the tokens declared by the top-level :root block (dark
// == false) or by the :root inside the top-level dark query (dark == true).
// The @supports fallback declares its own :root and is deliberately excluded:
// it patches two rungs for non-Apple platforms, it is not a mode block.
func rootTokens(decls []cssDecl, dark bool) map[string]string {
	out := map[string]string{}
	for _, d := range decls {
		if d.selector != ":root" || !strings.HasPrefix(d.prop, "--") {
			continue
		}
		switch {
		case !dark && len(d.atRules) == 0:
			out[d.prop] = d.value
		case dark && len(d.atRules) == 1 && d.atRules[0] == darkQuery:
			out[d.prop] = d.value
		}
	}
	return out
}

// TestCSSModeBlocksAgree fails when a colour token exists in one mode only.
// --keyColor, --shadow-small and --shadow-medium were all light-only, so dark
// mode ran a light-mode blue next to a remapped one and its 8%-black shadows
// resolved to a 2/255 shift — elevation that was not there at all, which is
// what the per-component dark repaints below used to compensate for.
func TestCSSModeBlocksAgree(t *testing.T) {
	decls := parseCSS()
	light, dark := rootTokens(decls, false), rootTokens(decls, true)

	if len(light) == 0 || len(dark) == 0 {
		t.Fatalf("parsed %d light and %d dark tokens; the parser lost a :root block", len(light), len(dark))
	}
	for name, value := range light {
		if colorValue.MatchString(value) && dark[name] == "" {
			t.Errorf("%s is a colour (%s) declared only in light mode", name, value)
		}
	}
	for name := range dark {
		if _, ok := light[name]; !ok {
			t.Errorf("%s is declared only in the dark block, so light mode has no value for it", name)
		}
	}
}

// TestCSSNoComponentDarkRules keeps dark mode in the token layer. A dark rule
// on a component is a token the layer could not express; the five that existed
// here all repainted a card because the shadow tokens were dead in dark.
func TestCSSNoComponentDarkRules(t *testing.T) {
	for _, d := range parseCSS() {
		inDark := false
		for _, at := range d.atRules {
			if at == darkQuery {
				inDark = true
			}
		}
		if inDark && d.selector != ":root" {
			t.Errorf("%s { %s } is a component rule inside %s; remap a token instead", d.selector, d.prop, darkQuery)
		}
	}
}

// TestCSSRadiiAreNamed rejects a literal border-radius. The set is
// {6, 9, 12, 17} plus the pill and the circle, and an orphan 8px had already
// appeared next to two structurally identical siblings using 12px.
func TestCSSRadiiAreNamed(t *testing.T) {
	// Each entry states why the literal is a shape rather than a value.
	allowed := map[string]string{}

	for _, d := range parseCSS() {
		if d.prop != "border-radius" || strings.Contains(d.value, "var(--") {
			continue
		}
		if _, ok := allowed[d.value]; ok {
			continue
		}
		t.Errorf("%s { border-radius: %s } is a literal; name it in the radius set", d.selector, d.value)
	}
}

// TestCSSFontSizesAreRelative rejects a px font-size. A px size ignores the
// reader's browser font-size setting outright, which is the whole point of the
// setting; the ramp is rem against a 16px root.
func TestCSSFontSizesAreRelative(t *testing.T) {
	// selector -> reason. Scoped to the selector so the exemption cannot
	// spread to the next rule that happens to want the same number.
	allowed := map[string]string{
		".field input, .field select": "iOS Safari zooms the page when a focused input computes under 16px, and rem would follow a shrunken root",
	}

	px := regexp.MustCompile(`(^|[\s/])[0-9]*\.?[0-9]+px`)
	font := regexp.MustCompile(`var\(--font-(family|mono)\)`)
	for _, d := range parseCSS() {
		// The ramp rungs are custom properties holding a font shorthand, not
		// font-size declarations. Checking only the latter reads as a passing
		// gate while the thirteen rungs it exists for sit in px.
		if d.prop != "font" && d.prop != "font-size" && !font.MatchString(d.value) {
			continue
		}
		if !px.MatchString(d.value) {
			continue
		}
		if _, ok := allowed[d.selector]; ok {
			continue
		}
		t.Errorf("%s { %s: %s } sizes type in px; use rem or add an allowlist entry with its reason", d.selector, d.prop, d.value)
	}
}

// TestCSSColorLiteralsAreTokens keeps colour choices in the token blocks. The
// failed pipeline chip carried a frozen light-mode red, so in dark it missed
// the alpha multiplier its neighbouring chips got and the stage an operator is
// hunting for rendered weaker than the ones either side of it.
func TestCSSColorLiteralsAreTokens(t *testing.T) {
	// Operands of a color-mix are an operation on a token, not a colour pick.
	// Inner var() references go first so the mix's own closing paren is found.
	ref := regexp.MustCompile(`var\([^)]*\)`)
	mix := regexp.MustCompile(`color-mix\([^)]*\)`)
	literal := regexp.MustCompile(`#[0-9a-fA-F]{3,8}\b|rgba?\(\s*[0-9]|hsla?\(\s*[0-9]`)

	for _, d := range parseCSS() {
		if d.selector == ":root" || strings.HasPrefix(d.prop, "--") {
			continue
		}
		if literal.MatchString(mix.ReplaceAllString(ref.ReplaceAllString(d.value, ""), "")) {
			t.Errorf("%s { %s: %s } picks a colour outside the token blocks", d.selector, d.prop, d.value)
		}
	}
}

// TestCSSGridCardsMayShrinkBelowTheirContent pins the escape from a grid item's
// automatic minimum size, which is its min-content width. .builder-card's
// heading is nowrap so it can be ellipsised, and that makes the card's
// min-content the entire untruncated title: the card refused to be narrower
// than its longest heading, the ellipsis never fired, and /monitor scrolled the
// whole document sideways at every viewport under 416px.
func TestCSSGridCardsMayShrinkBelowTheirContent(t *testing.T) {
	for _, d := range parseCSS() {
		if d.selector == ".builder-card" && d.prop == "min-width" && d.value == "0" {
			return
		}
	}
	t.Error(".builder-card does not declare min-width: 0, so the grid item " +
		"cannot be narrower than its nowrap heading and the page overflows")
}

// srgb is one colour with a straight (non-premultiplied) alpha, which is how a
// CSS colour value is written and how it composites onto what is behind it.
type srgb struct{ r, g, b, a float64 }

// parseColor reads the notations this stylesheet uses: #rgb/#rgba/#rrggbb/
// #rrggbbaa and the rgb()/rgba()/hsl()/hsla() functions, comma- or space-
// separated. It parses rather than pattern-matches because a regex that reaches
// for six hex digits or three leading numbers reads hsla(0, 0%, 100%, .85) as a
// colour it is not, and a contrast gate that is wrong about the numbers is
// worse than no gate: it certifies the ratio it failed to compute.
func parseColor(value string) (srgb, bool) {
	value = strings.TrimSpace(value)
	if hex, ok := strings.CutPrefix(value, "#"); ok {
		// The 3/4-digit forms expand each nibble to a byte (#abc == #aabbcc);
		// the 6/8-digit forms are already bytes.
		step := 0
		switch len(hex) {
		case 3, 4:
			step = 1
		case 6, 8:
			step = 2
		default:
			return srgb{}, false
		}
		out := srgb{a: 1}
		channels := []*float64{&out.r, &out.g, &out.b, &out.a}
		for index := 0; index*step < len(hex); index++ {
			digits := hex[index*step : (index+1)*step]
			if step == 1 {
				digits += digits
			}
			raw, err := strconv.ParseUint(digits, 16, 8)
			if err != nil {
				return srgb{}, false
			}
			*channels[index] = float64(raw) / 255
		}
		return out, true
	}
	name, arguments, ok := strings.Cut(value, "(")
	if !ok || !strings.HasSuffix(arguments, ")") {
		return srgb{}, false
	}
	args := strings.Fields(strings.NewReplacer(",", " ", "/", " ").
		Replace(strings.TrimSuffix(arguments, ")")))
	if len(args) < 3 || len(args) > 4 {
		return srgb{}, false
	}
	alpha := 1.0
	if len(args) == 4 {
		if alpha, ok = parseCSSNumber(args[3], 1); !ok {
			return srgb{}, false
		}
	}
	switch strings.TrimSpace(name) {
	case "rgb", "rgba":
		out := srgb{a: alpha}
		for index, channel := range []*float64{&out.r, &out.g, &out.b} {
			if *channel, ok = parseCSSNumber(args[index], 255); !ok {
				return srgb{}, false
			}
		}
		return out, true
	case "hsl", "hsla":
		hue, err := strconv.ParseFloat(strings.TrimSuffix(args[0], "deg"), 64)
		saturation, okS := parseCSSNumber(args[1], 1)
		lightness, okL := parseCSSNumber(args[2], 1)
		if err != nil || !okS || !okL {
			return srgb{}, false
		}
		red, green, blue := hslToRGB(hue, saturation, lightness)
		return srgb{red, green, blue, alpha}, true
	}
	return srgb{}, false
}

// parseCSSNumber returns a 0..1 channel from either a percentage or a plain
// number, where scale is what a plain number is measured against (255 for an
// rgb() channel, 1 for an alpha or an hsl() component).
func parseCSSNumber(text string, scale float64) (float64, bool) {
	if percent, ok := strings.CutSuffix(text, "%"); ok {
		value, err := strconv.ParseFloat(percent, 64)
		return value / 100, err == nil
	}
	value, err := strconv.ParseFloat(text, 64)
	return value / scale, err == nil
}

// hslToRGB is the conversion from the CSS Color 4 definition.
func hslToRGB(hue, saturation, lightness float64) (float64, float64, float64) {
	hue = math.Mod(math.Mod(hue, 360)+360, 360) / 30
	channel := func(offset float64) float64 {
		k := math.Mod(offset+hue, 12)
		a := saturation * math.Min(lightness, 1-lightness)
		return lightness - a*math.Max(-1, math.Min(math.Min(k-3, 9-k), 1))
	}
	return channel(0), channel(8), channel(4)
}

// over composites fg onto bg with the source-over operator CSS paints with, so
// a token carrying alpha is measured as what a reader actually sees rather than
// as its own straight colour.
func over(fg, bg srgb) srgb {
	alpha := fg.a + bg.a*(1-fg.a)
	if alpha == 0 {
		return srgb{}
	}
	mix := func(front, back float64) float64 {
		return (front*fg.a + back*bg.a*(1-fg.a)) / alpha
	}
	return srgb{mix(fg.r, bg.r), mix(fg.g, bg.g), mix(fg.b, bg.b), alpha}
}

func relativeLuminance(c srgb) float64 {
	linear := func(channel float64) float64 {
		if channel <= 0.04045 {
			return channel / 12.92
		}
		return math.Pow((channel+0.055)/1.055, 2.4)
	}
	return 0.2126*linear(c.r) + 0.7152*linear(c.g) + 0.0722*linear(c.b)
}

func contrastRatio(a, b srgb) float64 {
	high, low := relativeLuminance(a), relativeLuminance(b)
	if high < low {
		high, low = low, high
	}
	return (high + 0.05) / (low + 0.05)
}

// resolveToken follows a token whose whole value is a var() reference to another
// token in the same mode, so an indirection added later reads as the colour it
// resolves to rather than as a value the gate below cannot measure.
func resolveToken(tokens map[string]string, name string) (string, bool) {
	for hop := 0; hop < 8; hop++ {
		value, ok := tokens[name]
		if !ok {
			return "", false
		}
		before, after, isVar := strings.Cut(value, "var(")
		if !isVar || strings.TrimSpace(before) != "" {
			return value, true
		}
		reference, _, closed := strings.Cut(after, ")")
		if !closed {
			return "", false
		}
		name = strings.TrimSpace(strings.SplitN(reference, ",", 2)[0])
	}
	return "", false
}

func modeColor(t *testing.T, mode string, tokens map[string]string, name string) srgb {
	t.Helper()
	value, ok := resolveToken(tokens, name)
	if !ok {
		t.Fatalf("%s: %s resolves to no declared value", mode, name)
	}
	colour, ok := parseColor(value)
	if !ok {
		t.Fatalf("%s: %s = %q is not a colour this gate can measure", mode, name, value)
	}
	return colour
}

// TestCSSFocusRingClearsTheAccentButton pins 1.4.11 for the one pair that has
// almost no slack. A focused primary button paints --focusRing directly against
// --accentFill, and dark mode measures 3.07:1 against the 3:1 floor a non-text
// indicator has to clear: one step darker on --accentFill, or one step lighter
// on --focusRing, and the focused primary button stops being distinguishable
// with nothing on screen to say so.
func TestCSSFocusRingClearsTheAccentButton(t *testing.T) {
	decls := parseCSS()
	for _, mode := range []struct {
		name string
		dark bool
	}{{"light", false}, {"dark", true}} {
		tokens := rootTokens(decls, mode.dark)
		// Either token may grow an alpha, so each is composited onto what is
		// actually behind it — the raised card over the page floor, then the
		// button's fill, then the ring — rather than measured straight.
		ground := modeColor(t, mode.name, tokens, "--pageFloor")
		surface := over(modeColor(t, mode.name, tokens, "--pageRaised"), ground)
		fill := over(modeColor(t, mode.name, tokens, "--accentFill"), surface)
		ring := over(modeColor(t, mode.name, tokens, "--focusRing"), fill)
		if ratio := contrastRatio(ring, fill); ratio < 3 {
			t.Errorf("%s: --focusRing over --accentFill is %.2f:1, under the 3:1 "+
				"WCAG 1.4.11 floor for a non-text indicator", mode.name, ratio)
		}
	}
}

// A contrast ratio is a property of a PAIR the DOM composed, never of a token.
// Almost every ink in these blocks carries alpha and almost every surface under
// it does too, so measuring a token against its own declared value answers a
// question no reader ever asks. The comments in the token block were written
// that way — --keyColor is annotated "4.02:1 on #fff", which is a real number
// about #fff and not about --pageFloor, where the same ink measured 4.31:1 and
// the quiet button on top of its own tint wash measured 3.99:1. The four gates
// below assemble the ground first, one composite at a time, and only then ask
// for the ratio.

// modes is the two mode blocks, named so a failure says which one it is in.
var modes = []struct {
	name string
	dark bool
}{{"light", false}, {"dark", true}}

// surface is one ground a reader actually sees, already composited down to the
// page floor. The name is the chain that produced it, so a failure names the
// stack rather than a colour nobody wrote.
type surface struct {
	name string
	c    srgb
}

// modeSurfaces builds every ground text is set on in this stylesheet. The three
// bases are the page floor, the raised card over it and the sidebar rail over
// it; the alpha fills (--systemQuinary on a hovered row, a chip and a code
// block, --systemQuaternary on the stage and step cards, --systemGray6 on the
// log view, --controlBG inside a field) are composited onto whichever of those
// three they are actually painted over.
func modeSurfaces(t *testing.T, mode string, tokens map[string]string) []surface {
	t.Helper()
	floor := modeColor(t, mode, tokens, "--pageFloor")
	raised := over(modeColor(t, mode, tokens, "--pageRaised"), floor)
	nav := over(modeColor(t, mode, tokens, "--navSidebarBG"), floor)
	on := func(token string, ground surface) surface {
		return surface{token + " over " + ground.name, over(modeColor(t, mode, tokens, token), ground.c)}
	}
	base := []surface{{"--pageFloor", floor}, {"--pageRaised", raised}, {"--navSidebarBG", nav}}
	out := append([]surface(nil), base...)
	for _, ground := range base {
		out = append(out, on("--systemQuinary", ground))
	}
	// The quaternary cards and the log view only ever sit inside a card, and the
	// controls sit both inside one and on the bare page.
	out = append(out,
		on("--systemQuaternary", base[1]),
		on("--systemGray6", base[1]),
		on("--controlBG", base[1]),
		on("--controlBG", base[0]),
	)
	return out
}

// accentTint is the wash the stylesheet mixes under the quiet button, the
// current nav item and the current pipeline chip. It is a colour no token
// declares: it is --keyColor-rgb at an alpha the dark block scales through
// --alpha-multiplier, so it has to be assembled from both before anything can
// be measured standing on it.
func accentTint(t *testing.T, mode string, tokens map[string]string, alpha float64, ground srgb) srgb {
	t.Helper()
	channels, ok := resolveToken(tokens, "--keyColor-rgb")
	if !ok {
		t.Fatalf("%s: --keyColor-rgb is declared nowhere, so no tint can be assembled", mode)
	}
	multiplier, err := strconv.ParseFloat(strings.TrimSpace(tokens["--alpha-multiplier"]), 64)
	if err != nil {
		t.Fatalf("%s: --alpha-multiplier = %q is not a number", mode, tokens["--alpha-multiplier"])
	}
	wash, ok := parseColor(fmt.Sprintf("rgba(%s, %g)", channels, alpha*multiplier))
	if !ok {
		t.Fatalf("%s: rgba(%s, %g) is not a colour this gate can measure", mode, channels, alpha*multiplier)
	}
	return over(wash, ground)
}

// TestCSSTextInkClearsEverySurfaceItLandsOn is the body-text floor, 4.5:1, held
// against composited grounds rather than against #fff. --systemSecondary is the
// tight one: it clears by 0.04 on a quaternary card, which is the whole reason
// this measures the stack instead of the token — one more alpha layer under it
// and the rung is under the floor with nothing on screen to say so.
func TestCSSTextInkClearsEverySurfaceItLandsOn(t *testing.T) {
	decls := parseCSS()
	for _, mode := range modes {
		tokens := rootTokens(decls, mode.dark)
		surfaces := modeSurfaces(t, mode.name, tokens)
		for _, ink := range []string{"--systemPrimary", "--systemSecondary", "--successInk", "--dangerInk"} {
			colour := modeColor(t, mode.name, tokens, ink)
			for _, s := range surfaces {
				// The status inks dress an outcome sentence in a card, on the
				// page and in the sidebar rail; they never land on a fill.
				if strings.HasSuffix(ink, "Ink") && strings.Contains(s.name, " over ") {
					continue
				}
				if ratio := contrastRatio(over(colour, s.c), s.c); ratio < 4.5 {
					t.Errorf("%s: %s on %s is %.2f:1, under the 4.5:1 floor for body text",
						mode.name, ink, s.name, ratio)
				}
			}
		}
	}
}

// TestCSSAccentInkClearsTheGroundItIsWrittenOn pins the pair three separate
// readings converged on. A tint wash of the key colour is not a surface the key
// colour can be written on: --keyColor over its own 6% wash measured 3.99:1 on
// the page floor and its 8% wash in the sidebar 3.70:1, both under the floor
// 14px bold has, and the fix is an ink token for that ground rather than a
// darker value pasted into each of the four rules that needed one.
func TestCSSAccentInkClearsTheGroundItIsWrittenOn(t *testing.T) {
	decls := parseCSS()
	for _, mode := range modes {
		tokens := rootTokens(decls, mode.dark)
		floor := modeColor(t, mode.name, tokens, "--pageFloor")
		raised := over(modeColor(t, mode.name, tokens, "--pageRaised"), floor)
		nav := over(modeColor(t, mode.name, tokens, "--navSidebarBG"), floor)
		ink := modeColor(t, mode.name, tokens, "--onTintInk")
		// Every base alpha the stylesheet mixes the accent at: the quiet button
		// resting, active and hovered, and the current nav item.
		for _, alpha := range []float64{.06, .07, .08, .1} {
			for _, ground := range []surface{{"--pageRaised", raised}, {"--pageFloor", floor}, {"--navSidebarBG", nav}} {
				wash := accentTint(t, mode.name, tokens, alpha, ground.c)
				if ratio := contrastRatio(over(ink, wash), wash); ratio < 4.5 {
					t.Errorf("%s: --onTintInk on a %.0f%% accent wash over %s is %.2f:1, "+
						"under the 4.5:1 floor the button and nav label need",
						mode.name, alpha*100, ground.name, ratio)
				}
			}
		}
		// The filled half of the same pair: white on the accent, which is a
		// different token because the tint and the fill are different grounds.
		for _, ground := range []surface{{"--pageRaised", raised}, {"--pageFloor", floor}} {
			fill := over(modeColor(t, mode.name, tokens, "--accentFill"), ground.c)
			label := over(modeColor(t, mode.name, tokens, "--onAccentInk"), fill)
			if ratio := contrastRatio(label, fill); ratio < 4.5 {
				t.Errorf("%s: --onAccentInk on --accentFill over %s is %.2f:1, "+
					"under the 4.5:1 floor", mode.name, ground.name, ratio)
			}
		}
	}
}

// TestCSSControlBorderReadsAsAnEdge pins 1.4.11 for the boundary that IS the
// control. A field's fill is --controlBG and the card behind it is the same
// colour, so the 1px border is the only thing saying where the input starts;
// --labelDivider, which is right for a hairline between two table rows, measured
// 1.41:1 there. The edge is checked against both sides — its own fill and the
// page around it — because 3:1 against one of them is not a visible edge.
func TestCSSControlBorderReadsAsAnEdge(t *testing.T) {
	decls := parseCSS()
	for _, mode := range modes {
		tokens := rootTokens(decls, mode.dark)
		floor := modeColor(t, mode.name, tokens, "--pageFloor")
		raised := over(modeColor(t, mode.name, tokens, "--pageRaised"), floor)
		for _, ground := range []surface{{"--pageRaised", raised}, {"--pageFloor", floor}} {
			fill := over(modeColor(t, mode.name, tokens, "--controlBG"), ground.c)
			edge := over(modeColor(t, mode.name, tokens, "--controlBorder"), fill)
			for _, against := range []surface{{"--controlBG", fill}, {ground.name, ground.c}} {
				if ratio := contrastRatio(edge, against.c); ratio < 3 {
					t.Errorf("%s: --controlBorder on a control over %s is %.2f:1 against %s, "+
						"under the 3:1 WCAG 1.4.11 floor for the boundary that identifies the control",
						mode.name, ground.name, ratio, against.name)
				}
			}
		}
	}
}

// TestCSSPipelineFoldsInsteadOfPanning pins the row that could not be read.
// Nine stages in one nowrap flex row measured 1195px inside a 285px box at
// 375px: the failing Publish cell sat 793px past the right edge, two of the
// nine were visible, and the only thing saying the rest existed was a
// scrollbar. Two declarations carry the fix — the row folds, and the chip may
// break its own label rather than force the fold to fail.
// Red by: dropping flex-wrap from .pipeline, or putting white-space: nowrap
// back on .pipe-chip.
func TestCSSPipelineFoldsInsteadOfPanning(t *testing.T) {
	folds := false
	for _, d := range parseCSS() {
		if d.selector == ".pipeline" && d.prop == "flex-wrap" && d.value == "wrap" {
			folds = true
		}
		if d.selector == ".pipe-chip" && d.prop == "white-space" {
			t.Errorf(".pipe-chip { white-space: %s } stops the chip breaking its own "+
				"label, so a fold that cannot fit one chip overflows instead", d.value)
		}
	}
	if !folds {
		t.Error(".pipeline does not declare flex-wrap: wrap; nine stages then hold " +
			"one row wider than any viewport the console is read at")
	}
}

// TestCSSFailedSurfacesShareOneTreatment pins the pair. The failed stage was
// told from the eight that were not by nothing at all — same grey card, and an
// error block computing to rgb(255,255,255) with border 0px none, byte-
// identical to an ordinary card. The wash and the ink are declared once for the
// chip and reused, so a failure cannot read one way in the row and another way
// in the card under it.
// Red by: removing the failed stage card rule or the failure report's edge.
func TestCSSFailedSurfacesShareOneTreatment(t *testing.T) {
	want := map[string]string{
		".pipe-stage.failed .pipe-chip": "background",
		".stage-log-card.failed":        "background",
		".failure-report":               "border",
	}
	for _, d := range parseCSS() {
		prop, ok := want[d.selector]
		if !ok || d.prop != prop {
			continue
		}
		if !strings.Contains(d.value, "--systemRed") && !strings.Contains(d.value, "--dangerInk") {
			t.Errorf("%s { %s: %s } does not carry the failure token", d.selector, d.prop, d.value)
		}
		delete(want, d.selector)
	}
	for selector, prop := range want {
		t.Errorf("%s declares no %s, so a failure is not distinguished there", selector, prop)
	}
}

// TestCSSQuotaMeterSpeaksOnTwoChannels pins the rail's near-limit signal. The
// meter is the only thing in the console that says a build is about to be
// refused, and hue alone cannot say it: the level drives a bar colour AND a
// suffix on the value, so an operator who cannot separate orange from red still
// reads "9 / 10 !!". The bar's own height is part of the same reading — the
// whole point of a bar is the proportion between the fill and the track, and at
// three device pixels the fill's end could not be told from the track's.
// Red by: dropping either ::after rule, or thinning the bar back to 3px.
func TestCSSQuotaMeterSpeaksOnTwoChannels(t *testing.T) {
	suffixes := map[string]string{"warn": `" !"`, "crit": `" !!"`}
	hues := map[string]string{"warn": "--systemOrange", "crit": "--systemRed"}
	for _, d := range parseCSS() {
		for level := range suffixes {
			if d.selector == `.quota-meter[data-level="`+level+`"] .quota-v::after` && d.prop == "content" {
				if d.value != suffixes[level] {
					t.Errorf("the %s suffix is %s, not %s; the word channel is what a "+
						"reader who cannot use the hue is left with", level, d.value, suffixes[level])
				}
				delete(suffixes, level)
			}
			if d.selector == `.quota-meter[data-level="`+level+`"] .quota-bar i` && d.prop == "background" {
				if !strings.Contains(d.value, hues[level]) {
					t.Errorf("the %s bar is %s, not %s", level, d.value, hues[level])
				}
				delete(hues, level)
			}
		}
		if d.selector == ".quota-bar" && d.prop == "height" {
			height, err := strconv.ParseFloat(strings.TrimSuffix(d.value, "px"), 64)
			if err != nil || height < 6 {
				t.Errorf(".quota-bar { height: %s } is too thin to read as a ratio", d.value)
			}
		}
	}
	for level := range suffixes {
		t.Errorf("the %s level has no ::after suffix, so it is signalled by hue alone", level)
	}
	for level := range hues {
		t.Errorf("the %s level does not recolour its bar", level)
	}
	// The rail's numbers move under the reader when a count gains a digit;
	// tabular figures equalise digit width, not digit count.
	reserved := false
	for _, d := range parseCSS() {
		if d.selector == ".quota-v" && d.prop == "min-width" {
			reserved = true
		}
	}
	if !reserved {
		t.Error(".quota-v reserves no width, so the value shifts as the count gains a digit")
	}
}

// TestCSSReducedMotionIsHonoured pins both halves of honouring that preference:
// a reduce branch exists, and the one infinite animation is declared inside a
// no-preference query rather than clamped outside it. A Gentoo build runs for
// hours, so the pipeline pulse is the animation that never stops on its own.
func TestCSSReducedMotionIsHonoured(t *testing.T) {
	if !strings.Contains(appleCSS, "@media (prefers-reduced-motion: reduce)") {
		t.Error("appleCSS declares no prefers-reduced-motion: reduce branch")
	}
	for _, d := range parseCSS() {
		if d.prop != "animation" && d.prop != "animation-name" {
			continue
		}
		guarded := false
		for _, at := range d.atRules {
			if strings.Contains(at, "prefers-reduced-motion: no-preference") {
				guarded = true
			}
		}
		if !guarded {
			t.Errorf("%s { %s: %s } is declared outside a no-preference query, so reduce cannot remove it", d.selector, d.prop, d.value)
		}
	}
}
