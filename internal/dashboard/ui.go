package dashboard

// ui.go holds the dashboard's entire frontend: a shared Apple-style token
// foundation (values verbatim from Apple's shipped App Store stylesheet), a
// common app shell, i18n (English default, Chinese via toggle/auto-detect),
// and every page template. Pages are assembled at init time by appPage() so
// chrome stays consistent. All dynamic rendering uses createElement/
// textContent — no innerHTML interpolation of API data.

// appleCSS is served at /static/apple.css. Ink is an alpha ladder, surfaces
// are two levels (recessed floor / raised card), separation is hairlines, the
// only motion is a 210ms ease-out nudge, and dark mode is pure token
// remapping via prefers-color-scheme.
const appleCSS = `:root {
  color-scheme: light dark;
  --font-family: -apple-system, BlinkMacSystemFont, "Apple Color Emoji", "SF Pro", "PingFang SC", "SF Pro Icons", "Helvetica Neue", Helvetica, Arial, sans-serif;
  --font-mono: ui-monospace, "SF Mono", SFMono-Regular, Menlo, Consolas, monospace;

  /* Type ramp: Apple's rung structure, scaled up one step for dashboard
     readability (the original 13px body reads small for dense admin UI). */
  --header-emphasized:      700 38px/1.18 var(--font-family);
  --title-1-emphasized:     700 26px/1.2 var(--font-family);
  --title-2-emphasized:     700 20px/1.3 var(--font-family);
  --title-3-emphasized:     600 17px/1.35 var(--font-family);
  --title-3-tall:           400 17px/1.5 var(--font-family);
  --body:                   400 14px/1.35 var(--font-family);
  --body-emphasized:        600 14px/1.35 var(--font-family);
  --body-tall:              400 14px/1.5 var(--font-family);
  --body-bold-tall:         700 14px/1.4 var(--font-family);
  --callout:                400 13px/1.35 var(--font-family);
  --callout-emphasized:     600 13px/1.35 var(--font-family);
  --subhead-emphasized:     600 12px/1.3 var(--font-family);
  --footnote:               400 11px/1.35 var(--font-family);

  --systemPrimary:    rgba(0, 0, 0, .85);
  --systemSecondary:  rgba(0, 0, 0, .5);
  --systemTertiary:   rgba(0, 0, 0, .25);
  --systemQuaternary: rgba(0, 0, 0, .1);
  --systemQuinary:    rgba(0, 0, 0, .05);

  --systemRed:    #ff3b30;
  --systemOrange: #ff9500;
  --systemGreen:  #28cd41;
  --systemBlue:   #007aff;
  --systemGray:   #8e8e93;
  --systemGray6:  #f2f2f7;

  --keyColor: #007aff;
  --keyColor-rgb: 0, 122, 255;

  --pageFloor: #f5f5f7;
  --pageRaised: #fff;
  --navSidebarBG: rgba(60, 60, 67, .03);

  --labelDivider: rgba(0, 0, 0, .15);
  --keyline: .5px solid var(--labelDivider);
  --shadow-small:  0 3px  9px rgba(0, 0, 0, .08);
  --shadow-medium: 0 3px 20px rgba(0, 0, 0, .08);

  --radius-small: 9px;
  --radius-medium: 12px;
  --radius-large: 17px;
  --buttonRadius: 6px;

  --bodyGutter: 25px;
	--authCardWidth: 340px;
	--deviceCardWidth: 440px;
  --hover-transition: 210ms ease-out;
  --alpha-multiplier: 1;
}
@media (min-width: 1000px) { :root { --bodyGutter: 40px; } }

@media (prefers-color-scheme: dark) {
  :root {
    --systemPrimary:    hsla(0, 0%, 100%, .85);
    --systemSecondary:  hsla(0, 0%, 100%, .55);
    --systemTertiary:   hsla(0, 0%, 100%, .25);
    --systemQuaternary: hsla(0, 0%, 100%, .1);
    --systemQuinary:    hsla(0, 0%, 100%, .05);
    --systemRed:    #ff453a;
    --systemOrange: #ff9f0a;
    --systemGreen:  #32d74b;
    --systemBlue:   #0a84ff;
    --systemGray:   #98989d;
    --systemGray6:  #1c1c1e;
    --pageFloor: #151515;
    --pageRaised: #1f1f1f;
    --navSidebarBG: rgba(235, 235, 245, .03);
    --labelDivider: hsla(0, 0%, 100%, .1);
    --alpha-multiplier: 1.33;
  }
}
@supports not (font: -apple-system-body) {
  :root { --systemPrimary: rgba(0, 0, 0, .88); --systemSecondary: rgba(0, 0, 0, .56); }
  @media (prefers-color-scheme: dark) {
    :root { --systemPrimary: hsla(0, 0%, 100%, .92); --systemSecondary: hsla(0, 0%, 100%, .64); }
  }
}

* { margin: 0; padding: 0; box-sizing: border-box; }
[hidden] { display: none !important; }
body {
  background: var(--pageFloor);
  color: var(--systemPrimary);
  font: var(--body);
  letter-spacing: 0;
  font-synthesis: none;
  -webkit-font-smoothing: antialiased;
  -moz-osx-font-smoothing: grayscale;
}
a { color: var(--keyColor); text-decoration: none; }
:focus-visible { outline: 4px solid rgba(var(--keyColor-rgb), .6); outline-offset: 1px; }
@supports selector(:focus-visible) {
  a:focus, button:focus { box-shadow: none; outline: none; }
  a:focus-visible, button:focus-visible { box-shadow: 0 0 0 4px rgba(var(--keyColor-rgb), .6); outline: none; }
}

/* ---- pill buttons ---- */
.btn {
  border: 0; cursor: pointer; display: inline-block;
  border-radius: 1000px; padding: 7px 16px;
  font: var(--body-bold-tall); word-break: keep-all;
  transition: background-color .14s ease-out;
  background: rgba(var(--keyColor-rgb), calc(var(--alpha-multiplier) * .06));
  color: var(--keyColor);
}
.btn:hover  { background: rgba(var(--keyColor-rgb), calc(var(--alpha-multiplier) * .1)); transition: background-color .21s ease-out; }
.btn:active { background: rgba(var(--keyColor-rgb), calc(var(--alpha-multiplier) * .07)); }
.btn.blue        { background: var(--keyColor); color: hsla(0, 0%, 100%, .95); }
.btn.blue:hover  { background: color-mix(in srgb, var(--keyColor), #000 3%); }
.btn.blue:active { background: color-mix(in srgb, var(--keyColor), #000 6%); }
.btn.danger      { color: var(--systemRed); }
.btn[disabled] { opacity: .4; cursor: default; }
.btn[aria-disabled="true"] { opacity: .4; cursor: default; }
.lang-btn { border: 0; background: none; cursor: pointer; font: var(--callout); color: var(--systemSecondary); padding: 4px 8px; border-radius: 1000px; transition: background-color 175ms ease-in; }
.lang-btn:hover { background: var(--systemQuinary); }

/* ---- landing ---- */
.landing-nav {
  display: flex; align-items: center; justify-content: space-between;
  height: 48px; padding: 0 var(--bodyGutter);
  border-bottom: var(--keyline);
}
.landing-nav .brand { font: var(--body-emphasized); color: var(--systemPrimary); }
.landing-nav .side { display: flex; align-items: center; gap: 8px; }
.landing-nav .public-links { display: flex; align-items: center; gap: 4px; }
.landing-nav .public-link {
  color: var(--systemSecondary); font: var(--callout-emphasized);
  padding: 6px 9px; border-radius: var(--buttonRadius);
}
.landing-nav .public-link:hover, .landing-nav .public-link.active {
  background: var(--systemQuinary); color: var(--systemPrimary);
}
@media (max-width: 640px) {
  .landing-nav { height: auto; min-height: 48px; flex-wrap: wrap; padding-top: 7px; padding-bottom: 7px; }
  .landing-nav .public-links { order: 3; width: 100%; }
}
.landing-hero { max-width: 720px; margin: 0 auto; padding: 96px var(--bodyGutter) 64px; text-align: center; }
.landing-hero .eyebrow { font: var(--subhead-emphasized); text-transform: uppercase; letter-spacing: .06em; color: var(--systemSecondary); margin-bottom: 12px; }
.landing-hero h1 { font: var(--header-emphasized); text-wrap: balance; margin-bottom: 14px; }
.landing-hero .sub { font: var(--title-3-tall); color: var(--systemSecondary); text-wrap: pretty; margin-bottom: 28px; }
.landing-hero .cta { display: flex; gap: 10px; justify-content: center; }
.landing-grid {
  display: grid; grid-template-columns: repeat(3, 1fr); gap: 20px;
  max-width: 1000px; margin: 0 auto; padding: 0 var(--bodyGutter) 64px;
}
@media (max-width: 800px) { .landing-grid { grid-template-columns: 1fr; } }
.landing-card {
  background: var(--pageRaised); border-radius: var(--radius-large);
  box-shadow: var(--shadow-small); padding: 20px; min-height: 150px;
}
@media (prefers-color-scheme: dark) { .landing-card { background: var(--systemQuaternary); } }
.landing-card h4 { font: var(--subhead-emphasized); text-transform: uppercase; color: var(--systemSecondary); margin-bottom: 3px; }
.landing-card h3 { font: var(--title-2-emphasized); margin-bottom: 6px; }
.landing-card p  { font: var(--body-tall); color: var(--systemSecondary); text-wrap: pretty; }
.landing-flow { max-width: 1000px; margin: 0 auto; padding: 0 var(--bodyGutter) 80px; }
.landing-flow h2 { font: var(--title-2-emphasized); margin-bottom: 13px; }
.landing-flow .steps { display: grid; grid-template-columns: repeat(3, 1fr); gap: 20px; }
@media (max-width: 800px) { .landing-flow .steps { grid-template-columns: 1fr; } }
.landing-flow .step h4 { font: var(--subhead-emphasized); text-transform: uppercase; color: var(--systemSecondary); margin-bottom: 4px; }
.landing-flow .step p { font: var(--body-tall); color: var(--systemSecondary); }
.landing-flow .step .mono { font: 400 12.5px/1.6 var(--font-mono); color: var(--systemPrimary); background: var(--systemQuinary); border-radius: var(--buttonRadius); padding: 8px 10px; margin-top: 8px; display: block; overflow-x: auto; white-space: nowrap; }
.landing-footer { border-top: var(--keyline); padding: 20px var(--bodyGutter); font: var(--footnote); color: var(--systemTertiary); text-align: center; }

/* ---- public community pages ---- */
.public-main { max-width: 1080px; margin: 0 auto; padding: 54px var(--bodyGutter) 80px; }
.public-head { margin-bottom: 24px; max-width: 760px; }
.public-head h1 { font: var(--header-emphasized); margin-bottom: 8px; }
.public-head p { font: var(--title-3-tall); color: var(--systemSecondary); text-wrap: pretty; }
.public-search {
  display: grid; grid-template-columns: minmax(220px, 1fr) minmax(190px, 280px) auto;
  gap: 10px; align-items: end;
}
.public-search .field { margin: 0; }
.public-search input, .public-search select { min-height: 38px; }
.package-summary { padding: 13px 20px; color: var(--systemSecondary); border-bottom: var(--keyline); }
.package-name { font: 600 13px/1.4 var(--font-mono); }
.package-profile { max-width: 300px; overflow-wrap: anywhere; }
.package-flags { display: block; margin-top: 3px; font: var(--footnote); color: var(--systemTertiary); }
.public-pagination { display: flex; justify-content: space-between; align-items: center; padding: 14px 20px; border-top: var(--keyline); }
.status-overall { padding: 24px 20px; display: flex; align-items: center; justify-content: space-between; gap: 16px; }
.status-overall strong { display: block; font: var(--title-2-emphasized); margin-bottom: 4px; }
.status-overall p { color: var(--systemSecondary); }
.status-list { border-top: var(--keyline); }
.status-row { display: flex; justify-content: space-between; gap: 20px; padding: 15px 20px; border-bottom: var(--keyline); }
.status-row:last-child { border-bottom: 0; }
.status-row span:first-child { font: var(--body-emphasized); }
.public-docs { max-width: 820px; }
.public-docs .card-pad { padding: 24px; }
.public-docs .notice { padding: 12px 14px; background: var(--systemQuinary); border-radius: var(--radius-medium); margin: 12px 0 18px; color: var(--systemSecondary); }
.public-docs a { overflow-wrap: anywhere; }
@media (max-width: 700px) {
  .public-main { padding-top: 34px; }
  .public-search { grid-template-columns: 1fr; }
  .status-overall { align-items: flex-start; flex-direction: column; }
}

/* ---- auth card (login) ---- */
.auth-wrap { min-height: 100vh; display: flex; align-items: center; justify-content: center; padding: var(--bodyGutter); }
.auth-card {
  width: min(100%, var(--authCardWidth)); background: var(--pageRaised); border-radius: var(--radius-large);
  box-shadow: var(--shadow-medium); padding: 28px;
}
@media (prefers-color-scheme: dark) { .auth-card { background: var(--systemQuaternary); } }
.auth-card .brand { font: var(--subhead-emphasized); text-transform: uppercase; color: var(--systemSecondary); margin-bottom: 6px; }
.auth-card h1 { font: var(--title-1-emphasized); margin-bottom: 18px; }
.auth-card .btn { width: 100%; margin-top: 6px; }
.auth-card a.btn { display: inline-flex; justify-content: center; text-decoration: none; }
.auth-err { font: var(--callout); color: var(--systemRed); min-height: 15px; margin: 8px 0 2px; }
.auth-note { font: var(--callout); color: var(--systemTertiary); margin-top: 14px; text-align: center; display: flex; justify-content: center; gap: 10px; align-items: center; }
.auth-divider { display: flex; align-items: center; gap: 10px; margin: 14px 0 10px; color: var(--systemTertiary); font: var(--footnote); }
.auth-divider::before, .auth-divider::after { content: ""; flex: 1; border-top: var(--keyline); }
.device-card { width: min(100%, var(--deviceCardWidth)); }
.device-intro { color: var(--systemSecondary); font: var(--body-tall); margin-bottom: 18px; }
.device-code { font: var(--title-2-emphasized); font-family: var(--font-mono); letter-spacing: .08em; text-transform: uppercase; }
.device-identity { color: var(--systemSecondary); font: var(--callout); min-height: 18px; margin: 10px 0; overflow-wrap: anywhere; }
.device-actions { display: flex; flex-wrap: wrap; gap: 8px; }
.device-actions .btn { flex: 1 1 auto; margin-top: 0; }
.device-result { font: var(--callout); min-height: 18px; margin-top: 12px; overflow-wrap: anywhere; }
.device-result[data-state="error"] { color: var(--systemRed); }
.device-result[data-state="success"] { color: var(--systemGreen); }

/* ---- form fields ---- */
.field { margin-bottom: 12px; }
.field label { display: block; font: var(--callout-emphasized); color: var(--systemSecondary); margin-bottom: 4px; }
.field input[type=text], .field input[type=password], .field input[type=number], .field select {
  width: 100%; height: 32px; padding: 6px 8px;
  font: var(--body); font-family: var(--font-family); color: var(--systemPrimary);
  background: var(--pageRaised); border: 1px solid var(--labelDivider); border-radius: var(--buttonRadius);
}
@media (prefers-color-scheme: dark) { .field input, .field select { background: var(--systemQuinary); } }
.field input:focus, .field select:focus { box-shadow: 0 0 0 4px rgba(var(--keyColor-rgb), .6); outline: none; }
.field input::placeholder, .field textarea::placeholder { color: var(--systemTertiary); }
.field textarea {
  width: 100%; min-height: 120px; padding: 8px;
  font: 400 12.5px/1.6 var(--font-mono); color: var(--systemPrimary);
  background: var(--pageRaised); border: 1px solid var(--labelDivider); border-radius: var(--buttonRadius);
  resize: vertical;
}
@media (prefers-color-scheme: dark) { .field textarea { background: var(--systemQuinary); } }
.field textarea:focus { box-shadow: 0 0 0 4px rgba(var(--keyColor-rgb), .6); outline: none; }
.field .hint { font: var(--footnote); color: var(--systemTertiary); margin-top: 3px; }
.field.check { display: flex; align-items: center; gap: 8px; }
.field.check label { margin: 0; font: var(--body); color: var(--systemPrimary); }
@media (max-width: 483px) { .field input, .field select { font-size: 16px; height: 38px; border-radius: var(--radius-small); } }

/* ---- app shell ---- */
.shell { display: flex; min-height: 100vh; }
.sidebar {
  width: 260px; flex-shrink: 0;
  background: var(--navSidebarBG);
  border-inline-end: 1px solid var(--labelDivider);
  padding: 20px 12px;
  display: flex; flex-direction: column; gap: 2px;
  position: sticky; top: 0; height: 100vh;
}
.sidebar .brand { font: var(--body-emphasized); padding: 4px 8px 16px; color: var(--systemPrimary); }
.sidebar .brand span { display: block; font: var(--footnote); color: var(--systemTertiary); margin-top: 2px; }
.nav-item {
  display: block; padding: 8px; border-radius: var(--radius-medium);
  font: var(--body); color: var(--systemPrimary);
  overflow: hidden; text-overflow: ellipsis; white-space: nowrap;
  transition: background-color 175ms ease-in;
}
.nav-item:hover { background: var(--systemQuinary); }
.nav-item.active { background: rgba(var(--keyColor-rgb), calc(var(--alpha-multiplier) * .08)); color: var(--keyColor); font: var(--body-emphasized); }
.sidebar .spacer { flex: 1; }
.sidebar .foot { padding: 8px; font: var(--footnote); color: var(--systemTertiary); }
.sidebar .foot a { color: var(--systemSecondary); font: var(--callout); }
.project-context { padding: 10px 8px; border-top: 1px solid var(--labelDivider); }
.project-context label { display: block; font: var(--caption-1); color: var(--systemTertiary); margin-bottom: 5px; }
.project-context select {
  width: 100%; height: 32px; padding: 0 8px; color: var(--systemPrimary);
  background: var(--pageRaised); border: 1px solid var(--labelDivider); border-radius: var(--radius-small);
}
.project-context .identity { margin-top: 5px; font: var(--caption-2); color: var(--systemTertiary); overflow-wrap: anywhere; }

.content { flex: 1; min-width: 0; padding: 28px var(--bodyGutter) 60px; }
.page-head { display: flex; align-items: end; justify-content: space-between; gap: 12px; margin-bottom: 20px; flex-wrap: wrap; }
.page-head h1 { font: var(--title-1-emphasized); }
.page-head .sub { font: var(--callout); color: var(--systemSecondary); margin-top: 4px; }
.page-head .actions { display: flex; gap: 8px; align-items: center; }

.topbar { display: none; }
@media (max-width: 700px) {
  .shell { display: block; }
  .sidebar { display: none; }
  .topbar {
    display: flex; align-items: center; gap: 4px;
    position: sticky; top: 0; z-index: 100; height: 44px;
    padding: 0 12px; overflow-x: auto;
    background: var(--pageFloor);
    border-bottom: var(--keyline);
  }
  .topbar .brand { font: var(--body-emphasized); margin-right: 8px; white-space: nowrap; }
  .topbar .nav-item { padding: 6px 10px; border-radius: 1000px; white-space: nowrap; }
}

/* ---- cards, stats, tables ---- */
.card {
  background: var(--pageRaised); border-radius: var(--radius-large);
  box-shadow: var(--shadow-small); margin-bottom: 24px;
}
@media (prefers-color-scheme: dark) { .card { background: var(--systemQuaternary); } }
.card .card-pad { padding: 20px; }
.card h3.card-title { font: var(--title-3-emphasized); padding: 16px 20px 0; }
.section-title { font: var(--title-2-emphasized); margin: 0 0 13px; }

.stat-grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(160px, 1fr)); gap: 20px; margin-bottom: 24px; }
.stat-tile { background: var(--pageRaised); border-radius: var(--radius-large); box-shadow: var(--shadow-small); padding: 16px 20px; }
@media (prefers-color-scheme: dark) { .stat-tile { background: var(--systemQuaternary); } }
.stat-tile h4 { font: var(--subhead-emphasized); text-transform: uppercase; color: var(--systemSecondary); margin-bottom: 6px; }
.stat-tile .num { font: var(--title-1-emphasized); font-variant-numeric: tabular-nums; }
.stat-tile .num small { font: var(--callout); color: var(--systemTertiary); margin-left: 2px; }
.stat-tile .num.wrap { font: 500 13px/1.5 var(--font-mono); word-break: break-all; }

table.list { width: 100%; border-collapse: collapse; }
table.list th {
  font: var(--subhead-emphasized); text-transform: uppercase; color: var(--systemSecondary);
  text-align: left; padding: 12px 20px; border-bottom: var(--keyline);
}
table.list td { font: var(--body); padding: 11px 20px; border-bottom: var(--keyline); vertical-align: middle; }
table.list tr:last-child td { border-bottom: 0; }
table.list tbody tr { transition: background-color 175ms ease-in; }
table.list tbody tr:hover { background: var(--systemQuinary); }
table.list td.mono, .mono { font-family: var(--font-mono); font-size: 13px; }
table.list td.sec { color: var(--systemSecondary); }
.table-scroll { overflow-x: auto; }

.status { display: inline-flex; align-items: center; gap: 6px; font: var(--callout-emphasized); white-space: nowrap; }
.status .dot { width: 7px; height: 7px; border-radius: 50%; background: var(--status-color, var(--systemGray)); }
.status.green  { --status-color: var(--systemGreen); }
.status.blue   { --status-color: var(--systemBlue); }
.status.orange { --status-color: var(--systemOrange); }
.status.red    { --status-color: var(--systemRed); }
.status.gray   { --status-color: var(--systemGray); }

.empty { padding: 36px 20px; text-align: center; font: var(--callout); color: var(--systemTertiary); }
.provider-status-list { display: grid; gap: 8px; margin-top: 12px; }
.provider-status-row { padding: 10px 12px; border: 1px solid var(--separator); border-radius: 8px; font: var(--callout); }

pre.log-view {
  background: var(--systemGray6); border-radius: var(--radius-medium);
  padding: 16px; overflow: auto; max-height: 70vh;
  font: 400 12.5px/1.6 var(--font-mono); color: var(--systemPrimary);
  white-space: pre-wrap; word-break: break-word;
}

.builder-grid { display: grid; grid-template-columns: repeat(auto-fill, minmax(240px, 1fr)); gap: 20px; margin-bottom: 24px; }
.builder-card { background: var(--pageRaised); border-radius: var(--radius-large); box-shadow: var(--shadow-small); padding: 16px 20px; }
@media (prefers-color-scheme: dark) { .builder-card { background: var(--systemQuaternary); } }
.builder-card h3 { font: var(--title-3-emphasized); margin-bottom: 2px; overflow: hidden; text-overflow: ellipsis; }
.builder-card .ep { font: var(--callout); color: var(--systemSecondary); margin-bottom: 10px; overflow: hidden; text-overflow: ellipsis; }
.builder-card .meta { display: flex; justify-content: space-between; font: var(--callout); color: var(--systemSecondary); padding-top: 8px; border-top: var(--keyline); }
.ledger-grid { grid-template-columns: 1fr; }
.ledger-grid .builder-card .meta { justify-content: flex-start; gap: 6px 24px; flex-wrap: wrap; }
.target-card .meta {
  flex-direction: column; align-items: flex-start; justify-content: flex-start;
  gap: 5px; min-width: 0;
}
.target-card .meta span { max-width: 100%; overflow-wrap: anywhere; }

.form-grid { display: grid; grid-template-columns: 1fr 1fr; gap: 0 20px; }
@media (max-width: 800px) { .form-grid { grid-template-columns: 1fr; } }
.form-actions { display: flex; gap: 10px; align-items: center; margin-top: 8px; }
.save-msg { font: var(--callout); min-height: 15px; }
.save-msg.ok { color: var(--systemGreen); }
.save-msg.err { color: var(--systemRed); }
.policy-summary { margin-top: 7px; font: var(--footnote); color: var(--systemTertiary); line-height: 1.45; }
.policy-summary.suspended { color: var(--systemRed); }

.docs-body { max-width: 720px; }
.docs-body h2 { font: var(--title-2-emphasized); margin: 26px 0 10px; }
.docs-body h2:first-child { margin-top: 0; }
.docs-body p { font: var(--body-tall); color: var(--systemSecondary); margin-bottom: 8px; text-wrap: pretty; }
.docs-body pre { font: 400 12.5px/1.6 var(--font-mono); color: var(--systemPrimary); background: var(--systemQuinary); border-radius: var(--radius-medium); padding: 12px 14px; margin: 8px 0 14px; overflow-x: auto; }

/* ---- build pipeline ---- */
.pipeline { display: flex; align-items: center; gap: 0; overflow-x: auto; padding: 4px 0; }
.pipe-stage { display: flex; align-items: center; flex-shrink: 0; }
.pipe-chip {
  display: inline-flex; align-items: center; gap: 7px;
  padding: 7px 14px; border-radius: 1000px;
  font: var(--callout-emphasized); color: var(--systemSecondary);
  background: var(--systemQuinary); white-space: nowrap;
  transition: background-color 210ms ease-out;
}
.pipe-chip .dot { width: 7px; height: 7px; border-radius: 50%; background: var(--systemGray); }
.pipe-stage.done .pipe-chip { color: var(--systemPrimary); }
.pipe-stage.done .pipe-chip .dot { background: var(--systemGreen); }
.pipe-stage.current .pipe-chip { background: rgba(var(--keyColor-rgb), calc(var(--alpha-multiplier) * .1)); color: var(--keyColor); }
.pipe-stage.current .pipe-chip .dot { background: var(--systemBlue); animation: pipe-pulse 1.2s ease-in-out infinite; }
.pipe-stage.failed .pipe-chip { background: rgba(255, 59, 48, .1); color: var(--systemRed); }
.pipe-stage.failed .pipe-chip .dot { background: var(--systemRed); }
@keyframes pipe-pulse { 0%,100% { opacity: 1; } 50% { opacity: .35; } }
.pipe-arrow { width: 22px; height: 1px; background: var(--labelDivider); margin: 0 2px; flex-shrink: 0; }
.log-filters { display: flex; gap: 6px; margin-bottom: 10px; flex-wrap: wrap; }
.log-filters .btn { padding: 4px 12px; font: var(--callout-emphasized); }
.log-filters .btn.active { background: var(--keyColor); color: hsla(0, 0%, 100%, .95); }
.log-meta { display: flex; gap: 8px 18px; flex-wrap: wrap; margin: -2px 0 10px; font: var(--footnote); color: var(--systemTertiary); }
.stage-log-grid { display: grid; grid-template-columns: repeat(4, minmax(0, 1fr)); gap: 8px; margin-top: 14px; }
.stage-log-card { min-width: 0; padding: 10px 12px; border-radius: var(--radius-medium); background: var(--systemQuaternary); }
.stage-log-card strong { display: block; font: var(--callout-emphasized); }
.stage-log-card span { display: block; margin-top: 3px; font: var(--footnote); color: var(--systemSecondary); overflow-wrap: anywhere; }
@media (max-width: 900px) { .stage-log-grid { grid-template-columns: repeat(2, minmax(0, 1fr)); } }
@media (max-width: 520px) { .stage-log-grid { grid-template-columns: 1fr; } }

/* ---- settings sub-navigation ---- */
.settings-layout { display: flex; gap: 28px; align-items: flex-start; }
.subnav { width: 190px; flex-shrink: 0; display: flex; flex-direction: column; gap: 2px; position: sticky; top: 20px; }
.subnav a { display: block; padding: 8px 10px; border-radius: var(--radius-medium); font: var(--body); color: var(--systemPrimary); cursor: pointer; transition: background-color 175ms ease-in; }
.subnav a:hover { background: var(--systemQuinary); }
.subnav a.active { background: rgba(var(--keyColor-rgb), calc(var(--alpha-multiplier) * .08)); color: var(--keyColor); font: var(--body-emphasized); }
.subnav .subnav-label { font: var(--subhead-emphasized); text-transform: uppercase; color: var(--systemTertiary); padding: 12px 10px 4px; }
.settings-panels { flex: 1; min-width: 0; }
.panel { display: none; }
.panel.active { display: block; }
.settings-footer { position: sticky; bottom: 0; padding: 12px 0; background: var(--pageFloor); display: flex; gap: 10px; align-items: center; border-top: var(--keyline); }
@media (max-width: 800px) {
  .settings-layout { display: block; }
  .subnav { width: auto; flex-direction: row; overflow-x: auto; position: static; margin-bottom: 16px; }
  .subnav .subnav-label { display: none; }
  .subnav a { white-space: nowrap; }
}
.radio-row { display: flex; gap: 18px; margin: 2px 0 10px; flex-wrap: wrap; }
.radio-row label { display: flex; align-items: center; gap: 6px; font: var(--body); color: var(--systemPrimary); }
.card-actions { display: flex; gap: 10px; align-items: center; padding: 0 20px 16px; }
.factory-grid { display: grid; grid-template-columns: minmax(0, 1.4fr) minmax(260px, .6fr); gap: 20px; }
@media (max-width: 900px) { .factory-grid { grid-template-columns: 1fr; } }
.milestone-list { list-style: none; margin: 0; padding: 0; }
.milestone { display: grid; grid-template-columns: 110px minmax(0, 1fr); gap: 16px; padding: 16px 20px; border-bottom: var(--keyline); }
.milestone:last-child { border-bottom: 0; }
.milestone h3 { font: var(--body-emphasized); margin-bottom: 3px; }
.milestone p { font: var(--callout); color: var(--systemSecondary); }
.milestone-meta { margin-top: 5px; font: var(--footnote); color: var(--systemTertiary); }
.evidence-list { margin-top: 7px; display: flex; flex-wrap: wrap; gap: 6px 14px; }
.evidence-list span { font: 400 11.5px/1.5 var(--font-mono); color: var(--systemTertiary); overflow-wrap: anywhere; }
.factory-step-details { margin-top: 10px; border-top: var(--keyline); padding-top: 9px; }
.factory-step-details > summary { cursor: pointer; font: var(--callout-emphasized); color: var(--keyColor); }
.factory-step-list { margin-top: 9px; display: grid; gap: 8px; }
.factory-step { padding: 10px 12px; border-radius: var(--radius-medium); background: var(--systemQuaternary); }
.factory-step-head { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; }
.factory-step-head strong { font: var(--callout-emphasized); }
.factory-step-id { font: var(--footnote); color: var(--systemTertiary); }
.factory-step p { margin-top: 5px; }
.factory-step-log { margin-top: 7px; padding-top: 7px; border-top: var(--keyline); font: 400 11.5px/1.5 var(--font-mono); color: var(--systemTertiary); overflow-wrap: anywhere; }
.factory-note { font: var(--callout); color: var(--systemSecondary); line-height: 1.5; }
.blocker { padding: 13px 0; border-bottom: var(--keyline); }
.blocker:last-child { border-bottom: 0; }
.blocker strong { display: block; font: var(--body-emphasized); }
.blocker p { font: var(--callout); color: var(--systemSecondary); margin-top: 3px; }
`

// i18nJS: English is the default (and the text baked into the HTML); Chinese
// is applied by replacing textContent of [data-i18n] nodes. Language comes
// from localStorage, falling back to the browser language.
const i18nJS = `
var I18N = {
  zh: {
    'nav.overview': '总览', 'nav.builds': '构建任务', 'nav.monitor': '构建节点', 'nav.factory': '镜像工厂',
    'nav.settings': '设置', 'nav.packages': '软件包', 'nav.docs': '文档',
    'nav.status': '服务状态', 'nav.signout': '退出登录',
    'brand.sub': 'Gentoo Binhost 控制台',

    'title.landing': 'Portage Engine — 自托管 Gentoo 二进制包构建平台',
    'title.login': '登录 — Portage Engine',
	'title.device': 'CLI 设备授权 — Portage Engine',
    'title.overview': '总览 — Portage Engine',
    'title.builds': '构建任务 — Portage Engine',
    'title.detail': '构建详情 — Portage Engine',
    'title.logs': '构建日志 — Portage Engine',
    'title.monitor': '构建节点 — Portage Engine',
    'title.factory': '镜像工厂 — Portage Engine',
    'title.settings': '设置 — Portage Engine',
    'title.packages': '软件包 — Portage Engine',
    'title.docs': '文档 — Portage Engine',
    'title.status': '服务状态 — Portage Engine',

    'landing.signin': '登录控制台',
    'landing.eyebrow': 'Gentoo Binhost 构建平台',
    'landing.h1': '集中构建,处处安装',
    'landing.sub': '在 PVE 或云端按需拉起构建机,产物自动汇聚为 Portage 原生 binhost。客户端完成一次 binhost 与签名信任配置后,继续用 emerge 安装。',
    'landing.cta': '进入控制台',
    'landing.docs': '查看文档',
    'landing.packages': '软件包',
    'landing.status': '服务状态',
    'landing.f1.eyebrow': '按需构建机', 'landing.f1.title': '用完即毁的构建 VM',
    'landing.f1.text': '提交构建时在 Proxmox VE、GCP 或 AWS 创建全新的 Native Gentoo 构建虚拟机,按集群实时负载选择节点,任务完成后立即销毁。',
    'landing.f2.eyebrow': '原生 Binhost', 'landing.f2.title': '标准 Packages 索引',
    'landing.f2.text': '产物以 Portage 原生格式发布并支持 GPG 签名,任何 Gentoo 客户端配置一行 binrepos.conf 即可消费。',
    'landing.f3.eyebrow': '并行与汇聚', 'landing.f3.title': '多任务并行构建',
    'landing.f3.text': '多个构建任务各自独占虚拟机并行执行,产物统一回收进单一仓库,控制台实时跟踪每一步。',
    'landing.flow': '工作流',
    'landing.s1.t': '提交', 'landing.s1.d': '用客户端请求构建某个包,可附带策略允许的 Portage 配置子集。',
    'landing.s2.t': '构建', 'landing.s2.d': '服务端拉起构建机、在 Native Gentoo 或配置的容器中 emerge、回收产物并刷新索引。',
    'landing.s3.t': '消费', 'landing.s3.d': '任何 Gentoo 机器把本服务当 binhost,直接安装二进制包。',
    'landing.footer': 'Portage Engine · 自托管 Gentoo 二进制包构建平台',

    'login.h1': '登录', 'login.user': '用户名', 'login.pass': '密码',
    'login.submit': '登录', 'login.back': '返回首页',
    'login.oidc': '使用身份提供商登录', 'login.or': '或',
    'login.badcreds': '用户名或密码错误', 'login.fail': '登录失败', 'login.neterr': '网络错误:',
	'device.h1': '授权 CLI 登录',
	'device.intro': '核对终端显示的代码，再决定是否允许该 CLI 创建独立的短期平台会话。',
	'device.code': '授权代码', 'device.hint': '格式为八位字母或数字；中间连字符可省略。',
	'device.identity.loading': '正在确认当前平台身份…',
	'device.console': '控制台',
	'device.identity': '当前身份：', 'device.projects': '；授权项目数：',
	'device.federated.required': '请使用身份提供商登录；本地管理员会话不能批准 CLI 设备授权。',
	'device.identity.fail': '无法确认当前身份。', 'device.retry': '重试',
	'device.approve': '批准', 'device.deny': '拒绝',
	'device.invalid': '请输入有效的授权代码。', 'device.request.fail': '授权请求失败。',
	'device.approved': '已批准。可以安全返回终端。',
	'device.denied': '已拒绝。可以安全关闭此页面。',

    'common.refresh': '刷新', 'common.updated': '更新于 ',
    'common.loadfail': '加载失败:',
    'th.package': '包', 'th.version': '版本', 'th.arch': '架构', 'th.status': '状态',
    'th.jobid': '任务 ID', 'th.created': '创建时间', 'th.updated': '更新时间',

    'ov.h1': '总览', 'ov.recent': '最近构建',
    'ov.building': '构建中', 'ov.queued': '排队中', 'ov.instances': '云实例',
    'ov.total': '构建总数', 'ov.rate': '成功率',
    'ov.empty': '还没有构建任务。用 portage-client build 提交第一个吧。',

    'builds.h1': '构建任务', 'builds.count': '共 %d 个任务', 'builds.empty': '还没有构建任务。',

    'detail.h1': '构建详情', 'detail.logs': '查看日志', 'detail.error': '错误信息',
    'detail.livelog': '实时日志', 'detail.duration': '耗时',
    'detail.delete': '删除任务', 'detail.delete.confirm': '删除这条任务记录?',
    'detail.delete.fail': '删除失败:',
    'detail.cancel': '取消任务', 'detail.cancel.confirm': '取消这个任务并使当前执行器租约失效?',
    'detail.retry': '重试任务', 'detail.retry.confirm': '为这个任务创建新的隔离构建尝试?',
    'detail.action.fail': '操作失败:',
    'builds.cleanup': '清理失败任务', 'builds.cleanup.confirm': '移除所有失败的任务记录?',
    'pipe.queued': '排队', 'pipe.provision': '创建构建机', 'pipe.deploy': '部署 Builder',
    'pipe.build': '构建', 'pipe.collect': '隔离回收', 'pipe.verify': '安装验证', 'pipe.sign': '隔离签名', 'pipe.publish': '发布', 'pipe.cleanup': '释放实例',
    'filter.all': '全部', 'filter.queued': '排队', 'filter.provision': '供给', 'filter.deploy': '部署', 'filter.build': '构建',
    'filter.collect': '回收', 'filter.verify': '验证', 'filter.sign': '签名', 'filter.publish': '发布', 'filter.release': '释放',
    'detail.status': '状态', 'detail.arch': '架构', 'detail.created': '创建',
    'detail.updated': '更新', 'detail.instance': '实例', 'detail.artifact': '产物',
    'detail.unknown': '(未知)',

    'logs.h1': '构建日志', 'logs.back': '返回详情', 'logs.none': '(暂无日志)',
    'logs.fail': '日志加载失败:', 'logs.loading': '加载中…', 'logs.lines': '行',
    'logs.bytes': '日志大小', 'logs.generated': '刷新时间', 'logs.truncated': '日志已截断',
    'logs.last': '最后事件',

    'mon.h1': '构建节点', 'mon.sub': '静态 builder 与云实例',
    'mon.ledger': '任务账本', 'mon.ledger.shadow': 'PostgreSQL 任务真源',
    'mon.ledger.ok': '一致', 'mon.ledger.degraded': '异常',
    'mon.ledger.legacy': '进程投影视图 ', 'mon.ledger.rows': '数据库任务 ',
    'mon.ledger.repaired': '最近修复 ', 'mon.ledger.errors': '写入错误 ',
    'mon.ledger.checked': '最近核对 ',
    'mon.scheduler': '持久化调度器', 'mon.scheduler.pg': 'PostgreSQL 队列与租约',
    'mon.scheduler.healthy': '健康', 'mon.scheduler.degraded': '异常',
    'mon.scheduler.queue': '排队任务 ', 'mon.scheduler.unschedulable': '能力不匹配 ',
    'mon.scheduler.running': '运行任务 ',
    'mon.scheduler.leases': '有效租约 ', 'mon.scheduler.expired': '过期租约 ',
    'mon.scheduler.workers': '活跃执行槽 ', 'mon.scheduler.capability': '能力执行槽 ',
    'mon.scheduler.stale': '过期执行槽 ',
    'mon.scheduler.attempts': '最近一小时尝试 ',
    'mon.scheduler.fair.projects': '公平队列项目 ',
    'mon.scheduler.fair.starved': '反饥饿提升 ',
    'mon.scheduler.fair.dispatches': '公平派发 准入/阶段 ',
    'mon.scheduler.fair.maxwait': '已观测最长等待 ',
    'mon.scheduler.score.decisions': '软评分决策/多候选 ',
    'mon.scheduler.score.worker': 'Worker 决策 ',
    'mon.scheduler.autoscale': '扩缩容建议 ',
    'mon.scheduler.autoscale.slots': '槽位 活跃/期望 ',
    'mon.scheduler.autoscale.demand': '需求 繁忙/积压/不匹配 ',
    'mon.scheduler.autoscale.pools': '容量池 正常/阻塞 ',
    'mon.scheduler.autoscale.pool': '容量池 ',
    'mon.scheduler.autoscale.shadow': 'Phase 执行器处于 shadow；仅发布容量池清单，不产生扩缩容建议',
    'mon.scheduler.actuator': '执行器动作 开放/失败 ',
    'mon.scheduler.instances': '常驻实例 创建中/活跃/排空/删除 ',
    'mon.scheduler.action': '容量动作 ',
    'mon.scheduler.instance': '容量实例 ',
    'mon.targets': '目标可靠性与成本',
    'mon.targets.sub': '按项目、Profile、镜像代际与资源类别聚合',
    'mon.targets.empty': '最近 30 天没有终态样本',
    'mon.targets.samples': '样本 成功/失败/取消 ',
    'mon.targets.slo': '成功率 / SLO ',
    'mon.targets.latency': 'P50/P95 排队·运行 ',
    'mon.targets.cost': '预留/结算成本 ',
    'mon.targets.failure': '主要失败分类 ',
    'mon.targets.insufficient': '样本不足',
    'mon.gateway': 'Worker Gateway', 'mon.gateway.mtls': '出站拉取与短期 mTLS 身份',
    'mon.gateway.enabled': '已启用', 'mon.gateway.disabled': '兼容模式',
    'mon.gateway.connected': '已连接 ', 'mon.gateway.registered': '已登记 ',
    'mon.gateway.tasks': '待完成命令 ', 'mon.gateway.uploads': '待上传制品 ',
    'mon.gateway.inbound': '入站 Builder API ', 'mon.gateway.protocol': '执行协议 ',
    'mon.gateway.ttl': '证书 TTL ', 'mon.gateway.authority': '事实源 ',
    'mon.gateway.phase': 'Phase 执行器 ',
    'mon.metadata': '运行元数据', 'mon.metadata.db': 'Infra、制品与镜像工厂',
    'mon.metadata.infra': '存活实例 ', 'mon.metadata.cleanup': '清理失败 ',
    'mon.metadata.published': '已发布制品 ', 'mon.metadata.staged': '隔离制品 ',
    'mon.metadata.missing': '缺失制品 ', 'mon.metadata.corrupt': '损坏制品 ',
    'mon.metadata.orphaned': '孤儿制品 ',
    'mon.metadata.factory': '镜像工厂运行 ',
    'mon.cache': '实时加速层', 'mon.cache.redis': 'Redis Presence、限流与事件',
    'mon.cache.presence': '控制面实例 ', 'mon.cache.fallback': '故障时回退 PostgreSQL 轮询',
    'mon.builders': 'Builder', 'mon.instances': '云实例',
    'mon.noBuilders': '没有已注册的 builder。静态 builder 需配置 SERVER_URL 后自动注册;云构建的临时实例不在此列。',
    'mon.noInstances': '当前没有运行中的云实例。',
    'mon.archLabel': '架构 ', 'mon.loadLabel': '负载 ',
    'mon.policyLabel': '隔离策略 ', 'mon.accepting': '可接收任务',
    'mon.notAccepting': '等待回收，不再接收任务',
    'mon.shell': '终端',
    'factory.h1': '镜像工厂', 'factory.sub': 'Profile、离线输入、PVE/PBS 来源链与 E2E 证据',
    'factory.catalog': '已生效目录', 'factory.profiles': 'Profile', 'factory.images': '镜像',
    'factory.bundles': '离线输入包', 'factory.milestones': '里程碑',
    'factory.blockers': '阻塞项', 'factory.desktop': '桌面 E2E',
    'factory.notconfigured': '尚未配置 IMAGE_FACTORY_STATUS_PATH；目录数据可用，但里程碑证据不会由页面猜测。',
    'factory.noBlockers': '当前状态快照未报告阻塞项。',
    'factory.readonly': '此页面只读。发布与回滚仍通过签名 CLI 流程执行。',
    'factory.updated': '证据更新于 ', 'factory.sets': '包组', 'factory.image': '镜像', 'factory.displayModel': '显示硬件',
    'factory.channel': '通道', 'factory.default': '默认', 'factory.none': '无',
    'factory.evidence': '证据', 'factory.action': '下一步', 'factory.completed': '完成于',
    'factory.stepLogs': '阶段日志', 'factory.started': '开始', 'factory.finished': '结束',
    'factory.duration': '耗时', 'factory.log': '日志', 'factory.size': '大小',
    'factory.desktop.strategy': '策略', 'factory.desktop.ai': 'AI 使用边界',
    'factory.desktop.runner': '执行器', 'factory.desktop.display': '显示协议',
    'st.not_started': '未开始', 'st.planned': '已规划', 'st.in_progress': '进行中',
    'st.blocked': '受阻', 'st.passed': '已通过', 'st.failed': '失败',
    'set.sec.upload': '产物上传',
    'set.upload.desc': '配置后，只有通过隔离安装验证并完成中央发布的二进制包，才会连同 Packages 索引与签名公钥推送到内网镜像站。',
    'set.upload.url': '镜像站地址', 'set.upload.url.hint': '留空则不上传,包仅由本服务的 /binpkgs 提供',
    'set.upload.dir': '制品目录', 'set.upload.dir.hint': '文件位于 /local/<目录>/… 下,该 URL 即为内网 binhost',
    'set.upload.user': '用户名', 'set.upload.pass': '密码',
    'detail.artifact.deps': '个依赖包',
    'title.shell': '终端 — Portage Engine',
    'shell.back': '返回', 'shell.title': '实例终端',
    'shell.connected': '已连接', 'shell.closed': '已断开', 'shell.error': '连接错误',
    'th.instance': '实例', 'th.provider': '提供商', 'th.ip': 'IP',

    'set.h1': '设置', 'set.sub': '云构建配置在此管理,保存后立即生效并覆盖 server.conf',
    'set.cat.general': '通用', 'set.cat.infra': '基础设施', 'set.cat.access': '接入',
    'set.sec.mirrors': '镜像加速', 'set.sec.buildconf': '构建配置',
    'set.mirrors.hint': '拉起构建机时使用的内网镜像——局域网内可大幅加速部署。全部可选。',
    'set.mirrors.gentoo': 'Gentoo 镜像(GENTOO_MIRRORS)',
    'set.mirrors.gentoo.hint': 'distfiles 与 webrsync 快照;写入构建机的 make.conf',
    'set.mirrors.method': 'Portage 树同步方式',
    'set.mirrors.method.hint': 'webrsync 从 Gentoo 镜像下载一个快照包;rsync 逐文件同步,没有局域网 rsync 镜像时很慢',
    'set.mirrors.sync': 'Portage 同步 URI(可选)',
    'set.mirrors.sync.hint': '自定义 repos.conf sync-uri;留空则用 Gentoo 镜像的 webrsync 快照',
    'set.makeconf': '附加 make.conf 内容',
    'set.makeconf.hint': '逐字追加到每台构建机生成的 make.conf(全局 USE、ACCEPT_LICENSE、FEATURES、EMERGE_DEFAULT_OPTS 等)。包级 USE 由客户端配置包传递。',
    'set.buildfeatures': 'Native 构建 FEATURES',
    'set.buildfeatures.hint': '追加到 Native Gentoo 一次性构建 root 的 FEATURES;留空则使用镜像/profile 默认值。',
    'set.buildmode': '构建模式',
    'set.buildmode.hint': '构建后端固定为 Native Gentoo disposable root/VM;Docker builder 已移除。',
    'set.sec.general': '后端与测试', 'set.sec.builders': '静态 Builder',
    'set.sec.ssh': 'SSH 密钥', 'set.sec.net': '网络与投递', 'set.sec.gpg': 'GPG 签名',
    'set.gpg.state': '签名状态', 'set.gpg.keyid': '密钥 ID',
    'set.gpg.on': '已就绪', 'set.gpg.wait': '等待签名器', 'set.gpg.off': '未启用',
    'set.gpg.mode': '隔离模式', 'set.gpg.queue': '签名队列',
    'set.gpg.hint': '私钥只存在于独立 portage-signer 的专用卷中。控制面通过 PostgreSQL 提交摘要绑定任务,签名器主动拉取;Builder 与 WebUI 均不能读取或生成私钥。',
    'set.gpg.pubkey': '公钥', 'set.gpg.download': '下载公钥',
    'set.conn': '连接', 'set.placement': '节点调度', 'set.resources': '资源',
    'set.pveuser': '用户名(Token 的替代方式)',
    'set.pveuser.hint': 'user@realm 格式;两者都填时优先使用 Token',
    'set.pvepass': '密码',
    'set.place.auto': '自动调度(推荐)', 'set.place.manual': '指定节点',
    'set.place.auto.hint': '每次构建按集群实时负载自动落在最空闲的可用节点上',
    'set.provider.hint': '未配置静态 Builder 时使用;后续可扩展更多后端',
    'set.testbuild': '测试构建',
    'set.testbuild.hint': '用当前设置走完整流程:拉起构建机 → emerge 编译 → 产物回收进 binhost。',
    'set.testbuild.pkg': '包名(atom)',
    'set.testbuild.go': '发起测试构建',
    'set.testbuild.saving': '正在保存设置…',
    'set.testbuild.submitting': '正在提交构建…',
    'set.testbuild.submitted': '已提交,任务 ',
    'set.testbuild.view': '查看构建详情',
    'set.testbuild.fail': '测试构建失败:',
    'set.backend': '构建后端', 'set.provider': '默认提供商', 'set.ttl': '实例 TTL(分钟)',
    'set.ttl.hint': '实例最长生命周期和孤儿回收窗口;成功的 Native builder 立即销毁且不复用',
    'set.ttl': '实例闲置 TTL(分钟)',
    'set.verify': '每个 binpkg 必须先从任务隔离 binhost 完成安装验证，通过后才允许发布',
    'set.builders': '静态 Builder(逗号分隔 URL)',
    'set.builders.hint': '配置后构建轮询分发到这些 builder;留空则每次构建按需拉起云端临时 VM',
    'set.gcp': 'Google Cloud',
    'set.gcp.project': '项目', 'set.gcp.region': '区域', 'set.gcp.zone': '可用区',
    'set.gcp.keyfile': '服务账号密钥文件(服务端路径)',
    'set.aws': 'AWS',
    'set.aws.ak': 'Access Key ID', 'set.aws.sk': 'Secret Access Key',
    'set.pve': 'Proxmox VE',
    'set.endpoint': 'API 地址', 'set.node': '目标节点',
    'set.node.hint': '填 auto 时按集群实时负载自动选最空闲节点',
    'set.nodes': '候选节点(可选,逗号分隔)',
    'set.nodes.hint': '共享存储集群配合 auto 使用;留空则仅在持有模板的节点中选',
    'set.template': 'VM 模板',
    'set.template.hint': '须为装有 cloud-init 与 qemu-guest-agent 的 QEMU 模板',
    'set.nameserver': '构建机 DNS(可选)',
    'set.nameserver.hint': '经 cloud-init 下发;填内网 DNS 让构建机能解析镜像站/registry 域名',
    'set.cicustom': 'cloud-init snippet(cicustom,可选)',
    'set.cicustom.hint': '克隆后保留;基础镜像没有 qemu-guest-agent 时用 vendor snippet 首启安装',
    'set.tokenid': 'API Token ID', 'set.secret': 'API Token Secret',
    'set.secret.saved': '已保存;留空表示保持不变', 'set.secret.unset': '尚未设置',
    'set.secret.ph': '••••••••(留空保持不变)',
    'set.secret.external.set': '由部署环境提供（已配置）',
    'set.secret.external.unset': '由部署环境提供（尚未配置）',
    'set.storage': '存储池', 'set.bridge': '网桥',
    'set.tls': '跳过 TLS 证书校验(自签证书;更安全的做法是把 PVE CA 加入系统信任库)',
    'set.ssh': 'SSH 部署',
    'set.keypath': '私钥路径', 'set.keypath.hint': '对应 .pub 公钥经 cloud-init 注入新 VM',
    'set.sshuser': 'SSH 用户', 'set.sshuser.hint': '部署脚本需要 root 权限',
    'set.knownhosts': 'known_hosts 路径(可选)',
    'set.hostkey': '跳过 SSH host key 校验(新建 VM 首连必需,或改用上方 known_hosts)',
    'set.net': '网络与投递',
    'set.callback': '回调地址', 'set.callback.hint': '发布 Gate 必填：构建节点通过此地址读取任务隔离 binhost，必须从 builder 网络可达',
    'set.binpath': 'Builder 二进制路径', 'set.binpath.hint': '部署时 scp 到实例;需为 linux 且架构匹配',
    'set.binurl': 'Builder 二进制 URL(可选)', 'set.binurl.hint': '实例启动时自行下载;与路径同时设置时路径优先',
    'set.save': '保存', 'set.test': '测试 PVE 连接',
    'set.saving': '保存中…', 'set.saved': '已保存,立即生效', 'set.savefail': '保存失败:',
    'set.loadfail': '设置加载失败:',
    'set.testing': '正在连接 PVE…', 'set.testfail': '连接失败:',
    'set.testok': '连接成功,发现 %d 个节点',
    'set.clusternodes': '集群节点',
    'th.node': '节点', 'th.freemem': '空闲内存', 'th.cpu': 'CPU 负载', 'th.hastpl': '持有模板',
    'set.yes': '是', 'set.no': '否',

    'packages.h1': '软件包', 'packages.sub': '搜索已经发布到公开 Binhost 的二进制包。',
    'packages.search': '搜索', 'packages.search.ph': '包名、版本或 Profile',
    'packages.profile': 'Profile', 'packages.all': '全部 Profile', 'packages.default': '默认',
    'packages.download': '下载', 'packages.none': '没有找到匹配的软件包。',
    'packages.count': '共 %d 个已发布软件包', 'packages.prev': '上一页', 'packages.next': '下一页',
    'packages.page': '第 %d–%d 项', 'packages.loadfail': '软件包加载失败:',
    'docs.h1': '文档', 'docs.sub': '无需登录即可完成 Binhost 选择、签名信任和 Portage 配置。',
    'docs.consume': '消费二进制包',
    'docs.consume.p': '先在软件包页面确认与本机 ABI/Profile 匹配的 Binhost。每个 Profile 都是独立的 PKGDIR，不能把 /binpkgs 根目录当作聚合仓库。',
    'docs.client': '使用 Portage Engine 客户端',
    'docs.browse': '浏览已发布软件包和 Profile',
    'docs.client.p': '客户端会从公开 Profile 清单解析精确的官方风格路径，并写入 binrepos.conf。省略 profile-id 时选择默认 Profile：',
    'docs.manual': '手动配置 Portage',
    'docs.manual.p': '也可以直接创建 binrepos.conf。把示例路径替换为软件包页面显示的 Binhost 路径：',
    'docs.build': '请求构建',
    'docs.build.p': '浏览和安装已发布软件包不需要登录。只有提交构建请求需要登录，并选择有权限的项目：',
    'docs.build.p2': '构建在隔离环境完成安装验证和签名验证后才会发布；发布同时原子刷新对应 Profile 的 Packages 索引。',
    'docs.gpg': '建立签名信任',
    'docs.gpg.p': 'configure 只写 binrepos.conf，不会导入或信任密钥。必须从运营方控制的独立可信渠道取得发布公钥与完整指纹，导入 /etc/portage/gnupg 并建立信任；不要通过同一个未认证 HTTP 连接同时获取软件包和信任根。',
    'docs.verify.note': '网页中的“已发布”只表示制品进入公开仓库，不能替代客户端签名校验。',
    'status.h1': '服务状态', 'status.sub': '公开、脱敏的 Portage Engine 服务可用性。',
    'status.operational': '服务运行正常', 'status.degraded': '部分服务异常',
    'status.unavailable': '状态服务不可用', 'status.updated': '更新时间 ',
    'status.refresh': '自动每 30 秒刷新', 'status.version': '版本 ',
    'status.component.api': 'Web 与 API', 'status.component.repository': '软件包仓库',
    'status.component.build': '构建服务', 'status.state.operational': '正常',
    'status.state.degraded': '异常',

    'st.queued': '排队中', 'st.claimed': '已认领', 'st.provisioning': '开机中',
    'st.forwarding': '分发中', 'st.deploying': '部署中', 'st.building': '构建中',
    'st.collecting': '隔离回收中', 'st.verifying': '验证中', 'st.signing': '签名中', 'st.publishing': '发布中', 'st.success': '成功',
    'st.completed': '完成', 'st.failed': '失败', 'st.canceled': '已取消', 'st.online': '在线',
    'st.offline': '离线', 'st.running': '运行中', 'st.destroy_failed': '销毁失败'
  }
};
function peLang() {
  var saved = null;
  try { saved = localStorage.getItem('pe_lang'); } catch (e) {}
  if (saved === 'zh' || saved === 'en') return saved;
  return (navigator.language || '').toLowerCase().indexOf('zh') === 0 ? 'zh' : 'en';
}
function t(key, fallback) {
  var lang = peLang();
  if (lang !== 'en' && I18N[lang] && Object.prototype.hasOwnProperty.call(I18N[lang], key)) return I18N[lang][key];
  return fallback !== undefined ? fallback : key;
}
function applyI18n() {
  var lang = peLang();
  document.documentElement.lang = lang === 'zh' ? 'zh-CN' : 'en';
  var nodes = document.querySelectorAll('[data-i18n]');
  for (var i = 0; i < nodes.length; i++) {
    var n = nodes[i], key = n.getAttribute('data-i18n');
    if (!n.hasAttribute('data-i18n-default')) n.setAttribute('data-i18n-default', n.textContent);
    n.textContent = lang === 'en' ? n.getAttribute('data-i18n-default') : t(key, n.getAttribute('data-i18n-default'));
  }
  var toggles = document.querySelectorAll('.lang-btn');
  for (var j = 0; j < toggles.length; j++) toggles[j].textContent = lang === 'zh' ? 'English' : '中文';
}
function initLangToggles() {
  var toggles = document.querySelectorAll('.lang-btn');
  for (var i = 0; i < toggles.length; i++) {
    toggles[i].addEventListener('click', function () {
      try { localStorage.setItem('pe_lang', peLang() === 'zh' ? 'en' : 'zh'); } catch (e) {}
      applyI18n();
      if (typeof onLangChange === 'function') onLangChange();
    });
  }
}
applyI18n();
document.addEventListener('DOMContentLoaded', function () { applyI18n(); initLangToggles(); });
`

// baseJS: shared, injected into every shell page. DOM building goes through
// el()/textContent only — API data never reaches innerHTML.
const baseJS = `
function el(tag, cls, text) {
  var n = document.createElement(tag);
  if (cls) n.className = cls;
  if (text !== undefined && text !== null) n.textContent = String(text);
  return n;
}
function clear(node) { while (node.firstChild) node.removeChild(node.firstChild); }
async function api(path, opts) {
  if (typeof iamReady !== 'undefined' && iamReady) await iamReady;
  opts = opts || {};
  var stepUpRetry = !!opts._stepUpRetry;
  delete opts._stepUpRetry;
  var headers = new Headers(opts.headers || {});
  try {
    var projectID = localStorage.getItem('pe_project_id');
    if (projectID) headers.set('X-Project-ID', projectID);
  } catch (e) {}
  opts.headers = headers;
  var r = await fetch(path, opts);
  if (r.status === 401) { location.href = '/login'; throw new Error('unauthorized'); }
  if (!r.ok) {
    var msg = 'HTTP ' + r.status;
    var b = null;
    try { b = await r.json(); if (b && (b.details || b.error)) msg = b.details || b.error; } catch (e) {}
    if (r.status === 428 && b && b.code === 'step_up_required' && !stepUpRetry) {
      if (window.peAuthentication === 'oidc' ||
          window.peAuthentication === 'federated-session') {
        location.href = '/login?step_up=1';
        throw new Error('redirecting to fresh authentication');
      }
      var username = window.prompt('Administrator username');
      if (username === null) throw new Error(msg);
      var password = window.prompt('Re-enter administrator password');
      if (password === null) throw new Error(msg);
      var elevated = await fetch('/auth/step-up', {
        method: 'POST', headers: {'Content-Type': 'application/json'},
        body: JSON.stringify({username: username, password: password})
      });
      if (!elevated.ok) throw new Error('step-up authentication failed');
      var retry = Object.assign({}, opts, {_stepUpRetry: true});
      return api(path, retry);
    }
    throw new Error(msg);
  }
  return r.json();
}
async function initIAMContext() {
  var select = document.getElementById('project-switcher');
  var identity = document.getElementById('iam-identity');
  if (!select || !identity) return;
  try {
    var r = await fetch('/api/iam/me');
    if (!r.ok) throw new Error('HTTP ' + r.status);
    var data = await r.json();
    window.peAuthentication = data.principal && data.principal.authentication || '';
    var projects = Array.isArray(data.projects) ? data.projects : [];
    var saved = '';
    try { saved = localStorage.getItem('pe_project_id') || ''; } catch (e) {}
    clear(select);
    for (var i = 0; i < projects.length; i++) {
      var option = document.createElement('option');
      option.value = projects[i].project_id;
      option.textContent = projects[i].project_name + ' · ' + projects[i].role;
      select.appendChild(option);
    }
    if (saved && projects.some(function (p) { return p.project_id === saved; })) select.value = saved;
    if (!select.value && projects.length) select.value = projects[0].project_id;
    if (select.value) {
      try { localStorage.setItem('pe_project_id', select.value); } catch (e) {}
    }
    select.disabled = projects.length === 0;
    var p = data.principal || {};
    window.peIAM = {
      principal: p,
      projects: projects,
      identityProviders: Array.isArray(data.identity_providers) ? data.identity_providers : []
    };
    identity.textContent = (p.preferred_username || p.subject || '-') +
      (p.provider_id ? ' · ' + p.provider_id : '') +
      (p.system_admin ? ' · system-admin' : '');
    document.body.setAttribute('data-system-admin', p.system_admin ? 'true' : 'false');
    if (select.value) await loadProjectPolicySummary(select.value);
    if (!p.system_admin) {
      var adminLinks = document.querySelectorAll(
        'a[href="/monitor"],a[href="/image-factory"],a[href="/settings"]'
      );
      for (var j = 0; j < adminLinks.length; j++) adminLinks[j].remove();
      var adminActions = document.querySelectorAll('[data-system-admin]');
      for (var k = 0; k < adminActions.length; k++) adminActions[k].remove();
    }
    select.addEventListener('change', function () {
      try { localStorage.setItem('pe_project_id', select.value); } catch (e) {}
      location.reload();
    });
  } catch (err) {
    select.disabled = true;
    identity.textContent = 'IAM unavailable';
  }
}
async function loadProjectPolicySummary(projectID) {
  var summary = document.getElementById('project-policy-summary');
  if (!summary || !projectID) return;
  try {
    var headers = new Headers();
    headers.set('X-Project-ID', projectID);
    var response = await fetch('/api/projects/policy', {headers: headers});
    if (!response.ok) throw new Error('HTTP ' + response.status);
    var policy = await response.json();
    var budgetSuspended = !!policy.abuse_suspended;
    summary.classList.toggle('suspended', !!policy.suspended || budgetSuspended);
    summary.textContent = (policy.suspended ? 'Suspended · ' :
      (budgetSuspended ? 'Abuse cooldown · ' : '')) +
      'weight ' + (policy.priority_weight || 100) +
      ' · starvation ' + (policy.starvation_threshold_seconds || 300) + 's' +
      ' · ' +
      'queued ' + policy.queued_jobs + '/' + policy.max_queued_jobs +
      ' · active ' + policy.active_jobs + '/' + policy.max_active_jobs +
      ' · CPU ' + policy.reserved_vcpus + '/' + policy.max_active_vcpus +
      ' · RAM ' + policy.reserved_memory_mib + '/' + policy.max_active_memory_mib + ' MiB' +
      ' · disk ' + policy.reserved_disk_gib + '/' + policy.max_active_disk_gib + ' GiB' +
      ' · quarantine ' + fmtBytes(policy.quarantine_bytes) +
      ' (' + policy.active_artifact_budgets + ' jobs; ' +
      fmtBytes(policy.max_artifact_bytes_per_job) + '/job)' +
      ' · build ' + Math.ceil((policy.build_seconds_today || 0) / 60) +
      '/' + Math.ceil((policy.max_daily_build_seconds || 0) / 60) + ' min UTC' +
      ' · cloud μ ' + (policy.cloud_cost_microunits_today || 0) +
      '/' + (policy.max_daily_cloud_cost_microunits || 0) +
      ' (' + (policy.active_runtime_budgets || 0) + ' active)' +
      ' · failures ' + (policy.failures_last_hour || 0) +
      '/' + (policy.max_failures_per_hour || 0) + ' hour' +
      ' · phases c' + policy.claimed_reservations + '/' + policy.max_claimed_attempts +
      ' p' + policy.provision_reservations + '/' + policy.max_provision_attempts +
      ' b' + policy.build_reservations + '/' + policy.max_build_attempts +
      ' v' + policy.verify_reservations + '/' + policy.max_verify_attempts +
      ' pub' + policy.publish_reservations + '/' + policy.max_publish_attempts +
      ' wait' + policy.waiting_reservations +
      ' · plans active ' + policy.phase_work_active +
      ' shadow ' + policy.phase_work_shadow +
      ' · work ready ' + policy.phase_work_ready +
      ' unschedulable ' + (policy.phase_work_unschedulable || 0) +
      ' claimed ' + policy.phase_work_claimed +
      ' blocked ' + policy.phase_work_blocked +
      (policy.phase_work_failed ? ' failed ' + policy.phase_work_failed : '') +
      ' · UTC today ' + policy.submissions_today + '/' + policy.max_daily_submissions;
  } catch (err) {
    summary.textContent = 'Project quota unavailable';
  }
}
function hasProjectRole(required) {
  var ranks = { viewer: 1, developer: 2, maintainer: 3, owner: 4 };
  if (window.peIAM && window.peIAM.principal && window.peIAM.principal.system_admin) return true;
  var selected = '';
  try { selected = localStorage.getItem('pe_project_id') || ''; } catch (e) {}
  var projects = window.peIAM && Array.isArray(window.peIAM.projects) ? window.peIAM.projects : [];
  for (var i = 0; i < projects.length; i++) {
    if (projects[i].project_id === selected) return (ranks[projects[i].role] || 0) >= (ranks[required] || 99);
  }
  return false;
}
var iamReady = initIAMContext();
function fmtTime(s) {
  if (!s) return '-';
  var d = new Date(s);
  return isNaN(d) ? String(s) : d.toLocaleString();
}
function fmtBytes(n) {
  n = Number(n || 0);
  if (!isFinite(n) || n < 0) return '-';
  var units = ['B', 'KiB', 'MiB', 'GiB'];
  var i = 0;
  while (n >= 1024 && i < units.length - 1) { n /= 1024; i++; }
  return (i === 0 ? String(Math.round(n)) : n.toFixed(n >= 10 ? 1 : 2)) + ' ' + units[i];
}
function fmtTimeRange(start, end) {
  if (!start || !end) return '-';
  var ms = new Date(end) - new Date(start);
  if (!isFinite(ms) || ms < 0) return '-';
  var sec = Math.floor(ms / 1000);
  var h = Math.floor(sec / 3600), m = Math.floor((sec % 3600) / 60), s = sec % 60;
  return (h ? h + 'h ' : '') + (h || m ? m + 'm ' : '') + s + 's';
}
var STATUS_COLORS = {
  queued: 'gray', claimed: 'orange', provisioning: 'orange', forwarding: 'orange',
  deploying: 'orange', collecting: 'blue', verifying: 'blue', signing: 'blue', publishing: 'blue',
  building: 'blue', success: 'green', completed: 'green', failed: 'red', canceled: 'gray',
  online: 'green', busy: 'blue', draining: 'orange', offline: 'red', running: 'green', destroy_failed: 'red',
  not_started: 'gray', planned: 'gray', in_progress: 'blue', blocked: 'orange', passed: 'green'
};
function statusBadge(s) {
  var color = STATUS_COLORS[s] || 'gray';
  var wrap = el('span', 'status ' + color);
  wrap.appendChild(el('span', 'dot'));
  wrap.appendChild(el('span', null, t('st.' + s, s || '-')));
  return wrap;
}
function showError(containerId, err) {
  var c = document.getElementById(containerId);
  if (!c) return;
  clear(c);
  c.appendChild(el('div', 'empty', t('common.loadfail', 'Failed to load: ') + err.message));
}
`

// appPage assembles a full authed page: shared chrome + per-page content and
// script. active marks the nav item; titleKey is the i18n key for <title>.
func appPage(titleEN, titleKey, active, content, script string) string {
	nav := ""
	for _, it := range [][3]string{
		{"/overview", "Overview", "nav.overview"},
		{"/builds", "Builds", "nav.builds"},
		{"/monitor", "Build Nodes", "nav.monitor"},
		{"/image-factory", "Image Factory", "nav.factory"},
		{"/settings", "Settings", "nav.settings"},
		{"/packages", "Packages", "nav.packages"},
		{"/docs", "Docs", "nav.docs"},
		{"/status", "Status", "nav.status"},
	} {
		cls := "nav-item"
		if it[0] == "/"+active {
			cls += " active"
		}
		nav += `<a class="` + cls + `" href="` + it[0] + `" data-i18n="` + it[2] + `">` + it[1] + `</a>`
	}

	return `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title data-i18n="` + titleKey + `">` + titleEN + ` — Portage Engine</title>
<link rel="stylesheet" href="/static/apple.css?v=2">
</head>
<body>
<div class="topbar"><span class="brand">Portage Engine</span>` + nav + `</div>
<div class="shell">
  <nav class="sidebar" aria-label="Main navigation">
    <div class="brand">Portage Engine<span data-i18n="brand.sub">Gentoo binhost console</span></div>
    ` + nav + `
    <div class="spacer"></div>
    <div class="project-context">
      <label for="project-switcher">Project</label>
      <select id="project-switcher" aria-label="Active project"></select>
      <div class="identity" id="iam-identity">Loading identity…</div>
      <div class="policy-summary" id="project-policy-summary">Loading project quota…</div>
    </div>
    <div class="foot"><button class="lang-btn" type="button">中文</button></div>
    {{if .AuthEnabled}}<div class="foot"><a href="/logout" data-i18n="nav.signout">Sign Out</a></div>{{end}}
    <div class="foot">binhost:<span class="mono"> /binpkgs</span></div>
  </nav>
  <main class="content">
` + content + `
  </main>
</div>
<script>` + i18nJS + baseJS + script + `</script>
</body>
</html>`
}

const publicJS = `
function el(tag, cls, text) {
  var node = document.createElement(tag);
  if (cls) node.className = cls;
  if (text !== undefined && text !== null) node.textContent = String(text);
  return node;
}
function clear(node) { while (node.firstChild) node.removeChild(node.firstChild); }
async function publicJSON(path) {
  var response = await fetch(path, {credentials: 'omit', headers: {'Accept': 'application/json'}});
  if (!response.ok) throw new Error('HTTP ' + response.status);
  return response.json();
}
function fmtPublicTime(value) {
  var date = new Date(value);
  return isNaN(date) ? '-' : date.toLocaleString(peLang() === 'zh' ? 'zh-CN' : 'en');
}
function fmtPublicText(key, fallback) {
  var text = t(key, fallback);
  for (var i = 2; i < arguments.length; i++) text = text.replace('%d', String(arguments[i]));
  return text;
}
`

func publicPage(titleEN, titleKey, active, content, script string) string {
	links := ""
	for _, item := range [][3]string{
		{"/packages", "Packages", "nav.packages"},
		{"/docs", "Docs", "nav.docs"},
		{"/status", "Status", "nav.status"},
	} {
		className := "public-link"
		if item[0] == "/"+active {
			className += " active"
		}
		links += `<a class="` + className + `" href="` + item[0] +
			`" data-i18n="` + item[2] + `">` + item[1] + `</a>`
	}
	return `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title data-i18n="` + titleKey + `">` + titleEN + ` — Portage Engine</title>
<link rel="stylesheet" href="/static/apple.css?v=3">
</head>
<body>
<nav class="landing-nav" aria-label="Public navigation">
  <a class="brand" href="/">Portage Engine</a>
  <span class="public-links">` + links + `</span>
  <span class="side">
    <button class="lang-btn" type="button">中文</button>
    <a class="btn" href="/overview" data-i18n="landing.signin">Sign In</a>
  </span>
</nav>
<main class="public-main">` + content + `</main>
<footer class="landing-footer" data-i18n="landing.footer">Portage Engine · self-hosted Gentoo binary package platform</footer>
<script>` + i18nJS + publicJS + script + `</script>
</body>
</html>`
}

// ---------------------------------------------------------------------------
// Landing (public)
// ---------------------------------------------------------------------------

const landingHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title data-i18n="title.landing">Portage Engine — self-hosted Gentoo binary package platform</title>
<link rel="stylesheet" href="/static/apple.css?v=2">
</head>
<body>
<nav class="landing-nav">
  <a class="brand" href="/">Portage Engine</a>
  <span class="public-links">
    <a class="public-link" href="/packages" data-i18n="landing.packages">Packages</a>
    <a class="public-link" href="/docs" data-i18n="landing.docs">Documentation</a>
    <a class="public-link" href="/status" data-i18n="landing.status">Status</a>
  </span>
  <span class="side">
    <button class="lang-btn" type="button">中文</button>
    <a class="btn" href="/overview" data-i18n="landing.signin">Sign In</a>
  </span>
</nav>
<header class="landing-hero">
  <p class="eyebrow" data-i18n="landing.eyebrow">Gentoo binhost platform</p>
  <h1 data-i18n="landing.h1">Build once, install everywhere</h1>
  <p class="sub" data-i18n="landing.sub">Spin up build machines on demand on Proxmox VE or in the cloud. Artifacts converge into a native Portage binhost; after one-time binhost and signing-trust setup, clients continue to install with plain emerge.</p>
  <div class="cta">
    <a class="btn blue" href="/overview" data-i18n="landing.cta">Open Console</a>
    <a class="btn" href="/docs" data-i18n="landing.docs">Documentation</a>
  </div>
</header>
<section class="landing-grid" aria-label="Features">
  <article class="landing-card">
    <h4 data-i18n="landing.f1.eyebrow">On-demand builders</h4>
    <h3 data-i18n="landing.f1.title">Ephemeral build VMs</h3>
    <p data-i18n="landing.f1.text">A fresh native Gentoo build VM is created on Proxmox VE, GCP, or AWS, placed on an eligible node, and destroyed after the job.</p>
  </article>
  <article class="landing-card">
    <h4 data-i18n="landing.f2.eyebrow">Native binhost</h4>
    <h3 data-i18n="landing.f2.title">Standard Packages index</h3>
    <p data-i18n="landing.f2.text">Artifacts are published in Portage's native format with GPG signing. Any Gentoo client consumes them with one line of binrepos.conf.</p>
  </article>
  <article class="landing-card">
    <h4 data-i18n="landing.f3.eyebrow">Parallel and converged</h4>
    <h3 data-i18n="landing.f3.title">Concurrent builds</h3>
    <p data-i18n="landing.f3.text">Builds run in parallel, each on its own VM. Artifacts converge into a single repository, tracked live from the console.</p>
  </article>
</section>
<section class="landing-flow" aria-label="Workflow">
  <h2 data-i18n="landing.flow">Workflow</h2>
  <div class="steps">
    <div class="step">
      <h4 data-i18n="landing.s1.t">Submit</h4>
      <p data-i18n="landing.s1.d">Request a package build from the client, optionally with a policy-approved subset of your Portage configuration.</p>
      <span class="mono">portage-client build -package app-misc/jq</span>
    </div>
    <div class="step">
      <h4 data-i18n="landing.s2.t">Build</h4>
      <p data-i18n="landing.s2.d">The server provisions a builder, runs emerge on native Gentoo or in the configured container, collects the artifact, and refreshes the index.</p>
      <span class="mono">provision &rarr; emerge &rarr; collect &rarr; index</span>
    </div>
    <div class="step">
      <h4 data-i18n="landing.s3.t">Consume</h4>
      <p data-i18n="landing.s3.d">Any Gentoo machine points at this server as its binhost and installs binaries directly.</p>
      <span class="mono">emerge --getbinpkg app-misc/jq</span>
    </div>
  </div>
</section>
<footer class="landing-footer" data-i18n="landing.footer">Portage Engine · self-hosted Gentoo binary package platform</footer>
<script>` + i18nJS + `</script>
</body>
</html>`

// ---------------------------------------------------------------------------
// Login (public)
// ---------------------------------------------------------------------------

const loginHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title data-i18n="title.login">Sign In — Portage Engine</title>
<link rel="stylesheet" href="/static/apple.css?v=2">
</head>
<body>
<div class="auth-wrap">
  <div class="auth-card">
    <p class="brand">Portage Engine</p>
    <h1 data-i18n="login.h1">Sign In</h1>
    {{if .OIDCEnabled}}
    {{range .IdentityProviders}}
    <a class="btn blue" href="{{.LoginURL}}">Sign in with {{.DisplayName}}</a>
    {{end}}
    {{end}}
    {{if and .OIDCEnabled .LocalLoginEnabled}}
    <div class="auth-divider" data-i18n="login.or">or</div>
    {{end}}
    {{if .LocalLoginEnabled}}
    <form id="login-form" data-return-to="{{.ReturnTo}}">
    <div class="field">
      <label for="u" data-i18n="login.user">Username</label>
      <input type="text" id="u" autocomplete="username" autocapitalize="off" autocorrect="off" spellcheck="false">
    </div>
    <div class="field">
      <label for="p" data-i18n="login.pass">Password</label>
      <input type="password" id="p" autocomplete="current-password">
    </div>
    <p class="auth-err" id="err" role="status"></p>
    <button class="btn blue" type="submit" data-i18n="login.submit">Sign In</button>
    </form>
    {{end}}
    <p class="auth-note"><a href="/" data-i18n="login.back">Back to home</a><button class="lang-btn" type="button">中文</button></p>
  </div>
</div>
<script>` + i18nJS + `
{{if .LocalLoginEnabled}}
document.getElementById('login-form').addEventListener('submit', async function (e) {
  e.preventDefault();
  var err = document.getElementById('err');
  err.textContent = '';
  try {
	var returnTo = document.getElementById('login-form').getAttribute('data-return-to') || '';
	var loginURL = '/login' + (returnTo ? '?return_to=' + encodeURIComponent(returnTo) : '');
    var r = await fetch(loginURL, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        username: document.getElementById('u').value,
        password: document.getElementById('p').value
      })
    });
    if (!r.ok) {
      err.textContent = r.status === 401 ? t('login.badcreds', 'Wrong username or password')
                                         : t('login.fail', 'Sign-in failed') + ' (HTTP ' + r.status + ')';
      return;
    }
	var result = await r.json();
    location.href = result.redirect_to || '/overview';
  } catch (ex) { err.textContent = t('login.neterr', 'Network error: ') + ex.message; }
});
{{end}}
</script>
</body>
</html>`

// Device authorization is an authenticated operation page. It reuses the
// established auth-card shell, system theme and en/zh message layer. All
// human-readable slots wrap; the short opaque code stays fully visible.
const deviceAuthorizationHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title data-i18n="title.device">Authorize CLI — Portage Engine</title>
<link rel="stylesheet" href="/static/apple.css?v=4">
</head>
<body>
<div class="auth-wrap">
  <main class="auth-card device-card">
    <p class="brand">Portage Engine</p>
    <h1 data-i18n="device.h1">Authorize CLI sign-in</h1>
    <p class="device-intro" data-i18n="device.intro">Compare this code with the terminal before allowing the CLI to create its own short-lived platform session.</p>
    <form id="device-form" novalidate>
      <div class="field">
        <label for="device-code" data-i18n="device.code">Authorization code</label>
        <input class="device-code" type="text" id="device-code" value="{{.UserCode}}"
          autocomplete="one-time-code" autocapitalize="characters" autocorrect="off"
          spellcheck="false" maxlength="9" aria-describedby="device-hint device-result">
        <p class="hint" id="device-hint" data-i18n="device.hint">Eight letters or digits; the middle hyphen is optional.</p>
      </div>
      <p class="device-identity" id="device-identity" role="status">Checking the current platform identity…</p>
      <button class="btn" id="device-retry" type="button" hidden data-i18n="device.retry">Retry</button>
      <div class="device-actions">
        <button class="btn blue" id="device-approve" type="button" aria-disabled="true" data-i18n="device.approve">Approve</button>
        <button class="btn danger" id="device-deny" type="button" aria-disabled="true" data-i18n="device.deny">Deny</button>
      </div>
      <p class="device-result" id="device-result" role="alert"></p>
    </form>
    <p class="auth-note"><a href="/overview" data-i18n="device.console">Console</a><button class="lang-btn" type="button">中文</button></p>
  </main>
</div>
<script>` + i18nJS + `
(function () {
  var code = document.getElementById('device-code');
  var identity = document.getElementById('device-identity');
  var result = document.getElementById('device-result');
  var approve = document.getElementById('device-approve');
  var deny = document.getElementById('device-deny');
  var retry = document.getElementById('device-retry');
  var busy = false;
  var federated = false;
  var completed = false;
  var identityState = 'loading';
  var identityName = '';
  var identityProjectCount = 0;
  var resultCode = '';

  function normalizedCode() {
    var compact = code.value.toUpperCase().replace(/[^A-Z0-9]/g, '');
    if (!/^[A-HJ-NP-Z2-9]{8}$/.test(compact)) return '';
    return compact.slice(0, 4) + '-' + compact.slice(4);
  }
  function setActions(enabled) {
    approve.setAttribute('aria-disabled', enabled ? 'false' : 'true');
    deny.setAttribute('aria-disabled', enabled ? 'false' : 'true');
  }
  function renderIdentity() {
    if (identityState === 'ready') {
      identity.textContent = t('device.identity', 'Current identity: ') + identityName +
        t('device.projects', '; authorized projects: ') + String(identityProjectCount);
    } else if (identityState === 'required') {
      identity.textContent = t('device.federated.required', 'Sign in with an identity provider; a local administrator session cannot approve CLI device authorization.');
    } else if (identityState === 'failed') {
      identity.textContent = t('device.identity.fail', 'Could not confirm the current identity.');
    } else {
      identity.textContent = t('device.identity.loading', 'Checking the current platform identity…');
    }
  }
  function showResult(codeValue, state) {
    resultCode = codeValue;
    var message = '';
    if (resultCode === 'invalid') message = t('device.invalid', 'Enter a valid authorization code.');
    if (resultCode === 'request-fail') message = t('device.request.fail', 'Authorization request failed.');
    if (resultCode === 'approved') message = t('device.approved', 'Approved. You can safely return to the terminal.');
    if (resultCode === 'denied') message = t('device.denied', 'Denied. You can safely close this page.');
    result.textContent = message;
    result.setAttribute('data-state', state || '');
  }
  async function loadIdentity() {
    setActions(false);
    retry.hidden = true;
    identityState = 'loading';
    renderIdentity();
    try {
      var response = await fetch('/api/iam/me', {headers: {'Accept': 'application/json'}});
      if (!response.ok) throw new Error('HTTP ' + response.status);
      var body = await response.json();
      var principal = body.principal || {};
      federated = principal.authentication === 'federated-session';
      if (!federated) {
        identityState = 'required';
        renderIdentity();
        return;
      }
      identityName = principal.preferred_username || principal.subject || '-';
      identityProjectCount = Array.isArray(body.projects) ? body.projects.length : 0;
      identityState = 'ready';
      renderIdentity();
      setActions(true);
    } catch (error) {
      federated = false;
      identityState = 'failed';
      renderIdentity();
      retry.hidden = false;
    }
  }
  async function decide(decision) {
    if (busy || completed || !federated) return;
    var userCode = normalizedCode();
    if (!userCode) {
      code.setAttribute('aria-invalid', 'true');
      showResult('invalid', 'error');
      code.focus();
      return;
    }
    code.value = userCode;
    code.removeAttribute('aria-invalid');
    busy = true;
    setActions(false);
    showResult('', '');
    try {
      var response = await fetch('/api/iam/device/decision', {
        method: 'POST', headers: {'Content-Type': 'application/json', 'Accept': 'application/json'},
        body: JSON.stringify({user_code: userCode, decision: decision})
      });
      if (!response.ok) {
        var body = null;
        try { body = await response.json(); } catch (ignore) {}
        throw new Error(body && body.error ? body.error : 'HTTP ' + response.status);
      }
      showResult(decision === 'approve' ? 'approved' : 'denied', 'success');
      completed = true;
      code.readOnly = true;
    } catch (error) {
      showResult('request-fail', 'error');
      setActions(true);
    } finally {
      busy = false;
    }
  }
  approve.addEventListener('click', function () { decide('approve'); });
  deny.addEventListener('click', function () { decide('deny'); });
  retry.addEventListener('click', loadIdentity);
  window.onLangChange = function () {
    renderIdentity();
    showResult(resultCode, result.getAttribute('data-state'));
  };
  loadIdentity();
}());
</script>
</body>
</html>`

// ---------------------------------------------------------------------------
// Overview
// ---------------------------------------------------------------------------

const overviewContent = `
<div class="page-head">
  <div><h1 data-i18n="ov.h1">Overview</h1><p class="sub" id="updated"></p></div>
  <div class="actions"><button class="btn" id="refresh" data-i18n="common.refresh">Refresh</button></div>
</div>
<div class="stat-grid" id="stats"></div>
<h2 class="section-title" data-i18n="ov.recent">Recent Builds</h2>
<div class="card">
  <div class="table-scroll"><table class="list" aria-label="Recent builds">
    <thead><tr>
      <th data-i18n="th.package">Package</th><th data-i18n="th.version">Version</th>
      <th data-i18n="th.arch">Arch</th><th data-i18n="th.status">Status</th>
      <th data-i18n="th.updated">Updated</th>
    </tr></thead>
    <tbody id="recent"></tbody>
  </table></div>
  <div id="recent-empty"></div>
</div>`

const overviewJS = `
function statTile(labelKey, labelEN, value, suffix) {
  var tle = el('div', 'stat-tile');
  tle.appendChild(el('h4', null, t(labelKey, labelEN)));
  var n = el('div', 'num', value);
  if (suffix) n.appendChild(el('small', null, suffix));
  tle.appendChild(n);
  return tle;
}
async function load() {
  try {
    var s = await api('/api/status');
    var g = document.getElementById('stats');
    clear(g);
    g.appendChild(statTile('ov.building', 'Building', s.active_builds || 0));
    g.appendChild(statTile('ov.queued', 'Queued', s.queued_builds || 0));
    g.appendChild(statTile('ov.instances', 'Cloud Instances', s.active_instances || 0));
    g.appendChild(statTile('ov.total', 'Total Builds', s.total_builds || 0));
    g.appendChild(statTile('ov.rate', 'Success Rate', (s.success_rate || 0).toFixed(1), '%'));
    document.getElementById('updated').textContent = t('common.updated', 'Updated ') + fmtTime(s.last_updated);
  } catch (e) { showError('stats', e); }
  try {
    var builds = await api('/api/builds?limit=10');
    if (!Array.isArray(builds)) builds = builds.builds || [];
    var tb = document.getElementById('recent');
    var emptyBox = document.getElementById('recent-empty');
    clear(tb); clear(emptyBox);
    if (!builds.length) { emptyBox.appendChild(el('div', 'empty', t('ov.empty', 'No builds yet. Submit one with portage-client build.'))); return; }
    builds.forEach(function (b) {
      var tr = el('tr');
      var pkg = el('td');
      var a = el('a', null, b.package_name || t('detail.unknown', '(unknown)'));
      a.href = '/build/' + encodeURIComponent(b.job_id);
      pkg.appendChild(a);
      tr.appendChild(pkg);
      tr.appendChild(el('td', 'sec', b.version || '-'));
      tr.appendChild(el('td', 'sec', b.arch || '-'));
      var st = el('td'); st.appendChild(statusBadge(b.status)); tr.appendChild(st);
      tr.appendChild(el('td', 'sec', fmtTime(b.updated_at)));
      tb.appendChild(tr);
    });
  } catch (e) { showError('recent-empty', e); }
}
function onLangChange() { load(); }
document.getElementById('refresh').addEventListener('click', load);
load();
setInterval(load, 15000);
`

// ---------------------------------------------------------------------------
// Builds list
// ---------------------------------------------------------------------------

const buildsContent = `
<div class="page-head">
  <div><h1 data-i18n="builds.h1">Builds</h1><p class="sub" id="count"></p></div>
  <div class="actions">
    <button class="btn" id="cleanup-failed" data-system-admin data-i18n="builds.cleanup">Clean Up Failed</button>
    <button class="btn" id="refresh" data-i18n="common.refresh">Refresh</button>
  </div>
</div>
<div class="card">
  <div class="table-scroll"><table class="list" aria-label="Builds">
    <thead><tr>
      <th data-i18n="th.package">Package</th><th data-i18n="th.version">Version</th>
      <th data-i18n="th.arch">Arch</th><th data-i18n="th.status">Status</th>
      <th data-i18n="th.jobid">Job ID</th><th data-i18n="th.created">Created</th>
      <th data-i18n="th.updated">Updated</th>
    </tr></thead>
    <tbody id="rows"></tbody>
  </table></div>
  <div id="empty"></div>
</div>`

const buildsJS = `
async function load() {
  try {
    var builds = await api('/api/builds');
    if (!Array.isArray(builds)) builds = builds.builds || [];
    document.getElementById('count').textContent = t('builds.count', '%d jobs total').replace('%d', builds.length);
    var tb = document.getElementById('rows');
    var emptyBox = document.getElementById('empty');
    clear(tb); clear(emptyBox);
    if (!builds.length) { emptyBox.appendChild(el('div', 'empty', t('builds.empty', 'No builds yet.'))); return; }
    builds.forEach(function (b) {
      var tr = el('tr');
      var pkg = el('td');
      var a = el('a', null, b.package_name || t('detail.unknown', '(unknown)'));
      a.href = '/build/' + encodeURIComponent(b.job_id);
      pkg.appendChild(a);
      tr.appendChild(pkg);
      tr.appendChild(el('td', 'sec', b.version || '-'));
      tr.appendChild(el('td', 'sec', b.arch || '-'));
      var st = el('td'); st.appendChild(statusBadge(b.status)); tr.appendChild(st);
      var idTd = el('td', 'mono sec', (b.job_id || '').slice(0, 8));
      idTd.title = b.job_id || '';
      tr.appendChild(idTd);
      tr.appendChild(el('td', 'sec', fmtTime(b.created_at)));
      tr.appendChild(el('td', 'sec', fmtTime(b.updated_at)));
      tb.appendChild(tr);
    });
  } catch (e) { showError('empty', e); }
}
function onLangChange() { load(); }
document.getElementById('cleanup-failed').addEventListener('click', async function () {
  if (!confirm(t('builds.cleanup.confirm', 'Remove all failed job records?'))) return;
  try {
    var r = await api('/api/builds/cleanup-failed', { method: 'POST' });
    load();
  } catch (e) { alert(t('detail.delete.fail', 'Delete failed: ') + e.message); }
});
document.getElementById('refresh').addEventListener('click', load);
load();
setInterval(load, 15000);
`

// ---------------------------------------------------------------------------
// Build detail
// ---------------------------------------------------------------------------

const buildDetailContent = `
<div class="page-head" data-job-id="{{.JobID}}" id="head">
  <div><h1 id="title" data-i18n="detail.h1">Build Details</h1><p class="sub mono" id="jid"></p></div>
  <div class="actions">
    <a class="btn" id="logs-link" href="#" data-i18n="detail.logs">View Logs</a>
    <button class="btn" id="cancel-job" style="display:none" data-i18n="detail.cancel">Cancel Job</button>
    <button class="btn" id="retry-job" style="display:none" data-i18n="detail.retry">Retry Job</button>
    <button class="btn" id="delete-job" style="display:none" data-i18n="detail.delete">Delete Job</button>
    <button class="btn" id="refresh" data-i18n="common.refresh">Refresh</button>
  </div>
</div>
<div class="card"><div class="card-pad">
  <div class="pipeline" id="pipeline" aria-label="Build pipeline"></div>
  <div class="stage-log-grid" id="stage-log-summary"></div>
</div></div>
<div class="stat-grid" id="meta"></div>
<div class="card" id="err-card" style="display:none">
  <h3 class="card-title" data-i18n="detail.error">Error</h3>
  <div class="card-pad"><pre class="log-view" id="err-text"></pre></div>
</div>
<div class="card">
  <h3 class="card-title" data-i18n="detail.livelog">Live Log</h3>
  <div class="card-pad">
    <div class="log-filters" id="log-filters"></div>
    <div class="log-meta" id="live-log-meta"></div>
    <pre class="log-view" id="live-log">…</pre>
  </div>
</div>`

const buildDetailJS = `
var jobID = document.getElementById('head').getAttribute('data-job-id');
document.getElementById('jid').textContent = jobID;
document.getElementById('logs-link').href = '/logs/' + encodeURIComponent(jobID);
var lastDetail = null;
function fmtDuration(ms) {
  if (ms < 0) ms = 0;
  var s = Math.floor(ms / 1000);
  var h = Math.floor(s / 3600), m = Math.floor((s % 3600) / 60), sec = s % 60;
  return (h ? h + 'h ' : '') + (h || m ? m + 'm ' : '') + sec + 's';
}
function durationTile(b) {
  var terminal = b.status === 'failed' || b.status === 'completed' || b.status === 'success' || b.status === 'canceled';
  var end = terminal ? new Date(b.updated_at) : new Date();
  var tle = el('div', 'stat-tile');
  tle.appendChild(el('h4', null, t('detail.duration', 'Duration')));
  var v = el('div', 'num', fmtDuration(end - new Date(b.created_at)));
  v.id = 'duration-num';
  v.style.font = 'var(--title-3-emphasized)';
  v.style.fontVariantNumeric = 'tabular-nums';
  tle.appendChild(v);
  return tle;
}
setInterval(function () {
  var n = document.getElementById('duration-num');
  if (!n || !lastDetail) return;
  var b = lastDetail;
  var terminal = b.status === 'failed' || b.status === 'completed' || b.status === 'success' || b.status === 'canceled';
  if (!terminal) n.textContent = fmtDuration(new Date() - new Date(b.created_at));
}, 1000);
function metaTile(labelKey, labelEN, node, wrap) {
  var tle = el('div', 'stat-tile');
  tle.appendChild(el('h4', null, t(labelKey, labelEN)));
  var v = el('div', wrap ? 'num wrap' : 'num');
  if (!wrap) v.style.font = 'var(--title-3-emphasized)';
  if (typeof node === 'string') v.textContent = node; else v.appendChild(node);
  tle.appendChild(v);
  return tle;
}
function basename(p) { var i = (p || '').lastIndexOf('/'); return i >= 0 ? p.slice(i + 1) : p; }
(function () {
  var st = document.createElement('style');
  st.textContent = '.artifact-extra{margin-top:4px;font-size:11px;opacity:.85}.artifact-extra a{color:var(--keyColor)}.artifact-extra-note{margin-top:4px;font-size:11px;color:var(--systemSecondary)}';
  document.head.appendChild(st);
})();
async function load() {
  try {
    var b = await api('/api/builds/detail?job_id=' + encodeURIComponent(jobID));
    if (b.package_name) document.getElementById('title').textContent = b.package_name + (b.version ? ' ' + b.version : '');
    var g = document.getElementById('meta');
    clear(g);
    g.appendChild(metaTile('detail.status', 'Status', statusBadge(b.status)));
    g.appendChild(metaTile('detail.arch', 'Arch', b.arch || '-'));
    g.appendChild(metaTile('detail.created', 'Created', fmtTime(b.created_at)));
    g.appendChild(metaTile('detail.updated', 'Updated', fmtTime(b.updated_at)));
    lastDetail = b;
    g.appendChild(durationTile(b));
    if (b.instance_id) g.appendChild(metaTile('detail.instance', 'Instance', b.instance_id, true));
    var resolved = b.resolved_context || {};
    if (resolved.profile_id) g.appendChild(metaTile('detail.profile', 'Profile', resolved.profile_id, true));
    if (resolved.image_generation) g.appendChild(metaTile('detail.image', 'Image generation', (resolved.image_id || '-') + ' · ' + resolved.image_generation, true));
    if (resolved.egress_policy && resolved.egress_policy.id) {
      var egress = resolved.egress_policy.id + ' · ' + (resolved.egress_policy.mode || '-');
      if (resolved.egress_policy_digest) egress += ' · ' + resolved.egress_policy_digest.slice(0, 19) + '…';
      g.appendChild(metaTile('detail.egress', 'Egress policy', egress, true));
    }
    if (b.artifact_url) {
      var wrap = el('div');
      var a = el('a', null, basename(b.artifact_url));
      a.href = b.artifact_url;
      a.setAttribute('download', '');
      wrap.appendChild(a);
      var extras = (b.artifacts || []).filter(function (u) { return u !== b.artifact_url; });
      extras.forEach(function (u) {
        var row = el('div', 'artifact-extra');
        var ea = el('a', null, basename(u));
        ea.href = u;
        ea.setAttribute('download', '');
        row.appendChild(ea);
        wrap.appendChild(row);
      });
      if (extras.length) {
        var note = el('div', 'artifact-extra-note', '+' + extras.length + ' ' + t('detail.artifact.deps', 'dependency package(s)'));
        wrap.appendChild(note);
      }
      g.appendChild(metaTile('detail.artifact', 'Artifact', wrap, true));
    } else if (b.artifact_path) {
      g.appendChild(metaTile('detail.artifact', 'Artifact', basename(b.artifact_path), true));
    }
    var delBtn = document.getElementById('delete-job');
    var retryBtn = document.getElementById('retry-job');
    var cancelBtn = document.getElementById('cancel-job');
    var terminal = b.status === 'failed' || b.status === 'completed' || b.status === 'success' || b.status === 'canceled';
    var canMaintain = hasProjectRole('maintainer');
    delBtn.style.display = terminal && canMaintain ? '' : 'none';
    retryBtn.style.display = canMaintain && (b.status === 'failed' || b.status === 'canceled') ? '' : 'none';
    cancelBtn.style.display = !terminal && canMaintain ? '' : 'none';
    var errCard = document.getElementById('err-card');
    if (b.error) { errCard.style.display = ''; document.getElementById('err-text').textContent = b.error; }
    else errCard.style.display = 'none';
  } catch (e) { showError('meta', e); }
}
function onLangChange() { load(); renderLogs(); }

var STAGES = [
  { key: 'queued',    en: 'Queued' },
  { key: 'provision', en: 'Provision VM', marker: '[provision]' },
  { key: 'deploy',    en: 'Deploy Builder', marker: '[deploy]' },
  { key: 'build',     en: 'Build', marker: '[build] submitting' },
  { key: 'collect',   en: 'Collect Artifact', marker: '[collect]' },
  { key: 'verify',    en: 'Verify Install', marker: '[verify]' },
  { key: 'sign',      en: 'Isolated Sign', marker: '[sign]' },
  { key: 'publish',   en: 'Publish', marker: '[publish]' },
  { key: 'cleanup',   en: 'Release', marker: '[cleanup]' }
];
var FILTERS = [
  { key: 'all',       en: 'All',       prefixes: null },
  { key: 'queued',    en: 'Queued',    prefixes: ['[queued]'] },
  { key: 'provision', en: 'Provision', prefixes: ['[provision]', '[terraform]', '[policy]'] },
  { key: 'policy',    en: 'Network Policy', prefixes: ['[policy]'] },
  { key: 'deploy',    en: 'Deploy',    prefixes: ['[deploy]', '[remote]'] },
  { key: 'build',     en: 'Build',     prefixes: ['[build]'] },
  { key: 'collect',   en: 'Collect',   prefixes: ['[collect]'] },
  { key: 'verify',    en: 'Verify',    prefixes: ['[verify]'] },
  { key: 'sign',      en: 'Sign',      prefixes: ['[sign]'] },
  { key: 'publish',   en: 'Publish',   prefixes: ['[publish]'] },
  { key: 'release',   en: 'Release',   stage: 'cleanup', prefixes: ['[cleanup]'] }
];
var activeFilter = 'all';
var lastLogText = '';
var lastLogStages = [];
var lastLogMetadata = {};

function stageState(idx, reachedIdx, status, failedIdx, cleanupDone) {
  var terminal = status === 'completed' || status === 'success';
  if (terminal) return 'done';
  if (status === 'failed') {
    if (failedIdx >= 0) {
      if (idx === failedIdx) return 'failed';
      if (idx < failedIdx) return 'done';
      // The release stage still runs after a failure.
      if (STAGES[idx].key === 'cleanup' && cleanupDone) return 'done';
      return 'pending';
    }
    return idx === reachedIdx ? 'failed' : (idx < reachedIdx ? 'done' : 'pending');
  }
  if (idx < reachedIdx) return 'done';
  if (idx === reachedIdx) return 'current';
  return 'pending';
}
var STATUS_STAGE = { queued: 0, claimed: 0, provisioning: 1, deploying: 2, forwarding: 3, building: 3, collecting: 4, verifying: 5, signing: 6, publishing: 7 };
function renderPipeline(logText, status, failedStage) {
  var reached = 0;
  for (var i = 0; i < STAGES.length; i++) {
    if (STAGES[i].marker && logText.indexOf(STAGES[i].marker) !== -1) reached = i;
  }
  // Status is authoritative when it maps further than the (possibly truncated)
  // log markers.
  if (STATUS_STAGE[status] !== undefined && STATUS_STAGE[status] > reached) reached = STATUS_STAGE[status];
  if (status === 'completed' || status === 'success') reached = STAGES.length - 1;
  var failedIdx = -1;
  if (failedStage) {
    for (var j = 0; j < STAGES.length; j++) if (STAGES[j].key === failedStage) failedIdx = j;
  }
  var cleanupDone = logText.indexOf('[cleanup]') !== -1;
  var wrap = document.getElementById('pipeline');
  clear(wrap);
  STAGES.forEach(function (s, i) {
    var stage = el('span', 'pipe-stage ' + stageState(i, reached, status, failedIdx, cleanupDone));
    var chip = el('span', 'pipe-chip');
    chip.appendChild(el('span', 'dot'));
    chip.appendChild(el('span', null, t('pipe.' + s.key, s.en)));
    stage.appendChild(chip);
    wrap.appendChild(stage);
    if (i < STAGES.length - 1) wrap.appendChild(el('span', 'pipe-arrow'));
  });
}
function renderFilters() {
  var box = document.getElementById('log-filters');
  clear(box);
  FILTERS.forEach(function (f) {
    var count = f.key === 'all'
      ? lastLogText.split('\n').filter(function (line) { return line.trim(); }).length
      : ((lastLogStages.filter(function (stage) { return stage.id === (f.stage || f.key); })[0] || {}).line_count || 0);
    var b = el('button', 'btn' + (activeFilter === f.key ? ' active' : ''), t('filter.' + f.key, f.en) + ' (' + count + ')');
    b.type = 'button';
    b.addEventListener('click', function () { activeFilter = f.key; renderFilters(); renderLogs(); });
    box.appendChild(b);
  });
}
function renderStageLogSummary() {
  var box = document.getElementById('stage-log-summary');
  clear(box);
  lastLogStages.forEach(function (stage) {
    var def = STAGES.filter(function (item) { return item.key === stage.id; })[0];
    var card = el('div', 'stage-log-card');
    card.appendChild(el('strong', null, t('pipe.' + stage.id, def ? def.en : stage.id)));
    var timing = (stage.line_count || 0) + ' ' + t('logs.lines', 'line(s)');
    if (stage.started_at && stage.updated_at) timing += ' · ' + fmtTimeRange(stage.started_at, stage.updated_at);
    card.appendChild(el('span', null, timing));
    if (stage.last_message) card.appendChild(el('span', null, t('logs.last', 'Last event') + ': ' + stage.last_message.slice(0, 180)));
    box.appendChild(card);
  });
}
function renderLogMetadata() {
  var box = document.getElementById('live-log-meta');
  clear(box);
  box.appendChild(el('span', null, t('logs.bytes', 'Log size') + ': ' + fmtBytes(lastLogMetadata.bytes)));
  if (lastLogMetadata.generated_at) box.appendChild(el('span', null, t('logs.generated', 'Refreshed') + ': ' + fmtTime(lastLogMetadata.generated_at)));
  if (lastLogMetadata.truncated) box.appendChild(el('span', null, t('logs.truncated', 'Log truncated')));
}
function renderLogs() {
  var pre = document.getElementById('live-log');
  pre.removeAttribute('data-i18n');
  var f = FILTERS.filter(function (x) { return x.key === activeFilter; })[0];
  var lines = lastLogText.split('\n');
  if (f && f.prefixes) {
    lines = lines.filter(function (l) {
      return f.prefixes.some(function (p) { return l.indexOf(p) !== -1; });
    });
  }
  var atBottom = pre.scrollHeight - pre.scrollTop - pre.clientHeight < 40;
  pre.textContent = lines.join('\n') || t('logs.none', '(no logs yet)');
  if (atBottom) pre.scrollTop = pre.scrollHeight;
}
async function loadLogs() {
  try {
    var r = await api('/api/builds/logs?job_id=' + encodeURIComponent(jobID));
    lastLogText = r.logs || '';
    lastLogStages = r.stages || [];
    lastLogMetadata = r;
    renderFilters();
    renderLogs();
    renderStageLogSummary();
    renderLogMetadata();
    var d = await api('/api/builds/detail?job_id=' + encodeURIComponent(jobID));
    renderPipeline(lastLogText, d.status, d.failed_stage);
  } catch (e) { /* next tick */ }
}
renderFilters();
document.getElementById('delete-job').addEventListener('click', async function () {
  if (!confirm(t('detail.delete.confirm', 'Delete this job record?'))) return;
  try {
    await api('/api/builds/delete?job_id=' + encodeURIComponent(jobID), { method: 'DELETE' });
    location.href = '/builds';
  } catch (e) { alert(t('detail.delete.fail', 'Delete failed: ') + e.message); }
});
document.getElementById('cancel-job').addEventListener('click', async function () {
  if (!confirm(t('detail.cancel.confirm', 'Cancel this job and revoke its active executor lease?'))) return;
  try {
    await api('/api/builds/cancel?job_id=' + encodeURIComponent(jobID), { method: 'POST' });
    await load(); await loadLogs();
  } catch (e) { alert(t('detail.action.fail', 'Action failed: ') + e.message); }
});
document.getElementById('retry-job').addEventListener('click', async function () {
  if (!confirm(t('detail.retry.confirm', 'Create a new isolated attempt for this job?'))) return;
  try {
    await api('/api/builds/retry?job_id=' + encodeURIComponent(jobID), { method: 'POST' });
    await load(); await loadLogs();
  } catch (e) { alert(t('detail.action.fail', 'Action failed: ') + e.message); }
});
document.getElementById('refresh').addEventListener('click', function () { load(); loadLogs(); });
load();
loadLogs();
if (window.EventSource) {
  var eventProject = '';
  try { eventProject = localStorage.getItem('pe_project_id') || ''; } catch (e) {}
  var jobEvents = new EventSource('/api/events/jobs' +
    (eventProject ? '?project_id=' + encodeURIComponent(eventProject) : ''));
  jobEvents.addEventListener('job', function (event) {
    try {
      var update = JSON.parse(event.data);
      if (update.job_id === jobID) { load(); loadLogs(); }
    } catch (e) {}
  });
}
setInterval(load, 10000);
setInterval(loadLogs, 5000);
`

// ---------------------------------------------------------------------------
// Logs
// ---------------------------------------------------------------------------

const logsContent = `
<div class="page-head" data-job-id="{{.JobID}}" id="head">
  <div><h1 data-i18n="logs.h1">Build Logs</h1><p class="sub mono" id="jid"></p></div>
  <div class="actions">
    <a class="btn" id="back-link" href="#" data-i18n="logs.back">Back to Details</a>
    <button class="btn" id="refresh" data-i18n="common.refresh">Refresh</button>
  </div>
</div>
<div class="card"><div class="card-pad">
  <div class="log-filters" id="log-filters"></div>
  <div class="log-meta" id="log-meta"></div>
  <div class="stage-log-grid" id="stage-log-summary"></div>
  <pre class="log-view" id="log" data-i18n="logs.loading">Loading…</pre>
</div></div>`

const logsJS = `
var jobID = document.getElementById('head').getAttribute('data-job-id');
document.getElementById('jid').textContent = jobID;
document.getElementById('back-link').href = '/build/' + encodeURIComponent(jobID);
var LOG_FILTERS = [
  { key: 'all', en: 'All', prefixes: null },
  { key: 'queued', en: 'Queued', prefixes: ['[queued]'] },
  { key: 'provision', en: 'Provision', prefixes: ['[provision]', '[terraform]', '[policy]'] },
  { key: 'policy', en: 'Network Policy', prefixes: ['[policy]'] },
  { key: 'deploy', en: 'Deploy', prefixes: ['[deploy]', '[remote]'] },
  { key: 'build', en: 'Build', prefixes: ['[build]'] },
  { key: 'collect', en: 'Collect', prefixes: ['[collect]'] },
  { key: 'verify', en: 'Verify', prefixes: ['[verify]'] },
  { key: 'release', en: 'Release', stage: 'cleanup', prefixes: ['[cleanup]'] }
];
var activeLogFilter = 'all';
var fullLogText = '';
var fullLogStages = [];
var fullLogMetadata = {};
function renderFullLogFilters() {
  var box = document.getElementById('log-filters'); clear(box);
  LOG_FILTERS.forEach(function (filter) {
    var count = filter.key === 'all'
      ? fullLogText.split('\n').filter(function (line) { return line.trim(); }).length
      : ((fullLogStages.filter(function (stage) { return stage.id === (filter.stage || filter.key); })[0] || {}).line_count || 0);
    var button = el('button', 'btn' + (activeLogFilter === filter.key ? ' active' : ''), t('filter.' + filter.key, filter.en) + ' (' + count + ')');
    button.type = 'button';
    button.addEventListener('click', function () { activeLogFilter = filter.key; renderFullLogFilters(); renderFullLog(); });
    box.appendChild(button);
  });
}
function renderFullLog() {
  var filter = LOG_FILTERS.filter(function (item) { return item.key === activeLogFilter; })[0];
  var lines = fullLogText.split('\n');
  if (filter && filter.prefixes) {
    lines = lines.filter(function (line) {
      return filter.prefixes.some(function (prefix) { return line.indexOf(prefix) !== -1; });
    });
  }
  document.getElementById('log').textContent = lines.join('\n') || t('logs.none', '(no logs yet)');
}
function renderFullLogDetails() {
  var meta = document.getElementById('log-meta'); clear(meta);
  meta.appendChild(el('span', null, t('logs.bytes', 'Log size') + ': ' + fmtBytes(fullLogMetadata.bytes)));
  if (fullLogMetadata.generated_at) meta.appendChild(el('span', null, t('logs.generated', 'Refreshed') + ': ' + fmtTime(fullLogMetadata.generated_at)));
  if (fullLogMetadata.truncated) meta.appendChild(el('span', null, t('logs.truncated', 'Log truncated')));
  var grid = document.getElementById('stage-log-summary'); clear(grid);
  fullLogStages.forEach(function (stage) {
    var card = el('div', 'stage-log-card');
    card.appendChild(el('strong', null, t('pipe.' + stage.id, stage.id)));
    var timing = (stage.line_count || 0) + ' ' + t('logs.lines', 'line(s)');
    if (stage.started_at && stage.updated_at) timing += ' · ' + fmtTimeRange(stage.started_at, stage.updated_at);
    card.appendChild(el('span', null, timing));
    if (stage.last_message) card.appendChild(el('span', null, t('logs.last', 'Last event') + ': ' + stage.last_message.slice(0, 180)));
    grid.appendChild(card);
  });
}
async function load() {
  var pre = document.getElementById('log');
  pre.removeAttribute('data-i18n');
  try {
    var r = await api('/api/builds/logs?job_id=' + encodeURIComponent(jobID));
    fullLogText = r.logs || '';
    fullLogStages = r.stages || [];
    fullLogMetadata = r;
    renderFullLogFilters();
    renderFullLogDetails();
    renderFullLog();
  } catch (e) { pre.textContent = t('logs.fail', 'Failed to load logs: ') + e.message; }
}
function onLangChange() { renderFullLogFilters(); renderFullLogDetails(); renderFullLog(); }
document.getElementById('refresh').addEventListener('click', load);
load();
setInterval(load, 5000);
`

// ---------------------------------------------------------------------------
// Monitor (builders + instances)
// ---------------------------------------------------------------------------

const monitorContent = `
<div class="page-head">
  <div><h1 data-i18n="mon.h1">Build Nodes</h1><p class="sub" data-i18n="mon.sub">Static builders and cloud instances</p></div>
  <div class="actions"><button class="btn" id="refresh" data-i18n="common.refresh">Refresh</button></div>
</div>
<h2 class="section-title" data-i18n="mon.ledger">Job Ledger</h2>
<div class="builder-grid ledger-grid" id="ledger"></div>
<div id="ledger-error"></div>
<h2 class="section-title" data-i18n="mon.scheduler">Durable Scheduler</h2>
<div class="builder-grid ledger-grid" id="scheduler"></div>
<div id="scheduler-error"></div>
<h2 class="section-title" data-i18n="mon.targets">Target Reliability and Cost</h2>
<p class="sub" data-i18n="mon.targets.sub">Grouped by project, profile, image generation, and resource class</p>
<div class="builder-grid" id="target-history"></div>
<div id="target-history-empty"></div>
<h2 class="section-title" data-i18n="mon.gateway">Worker Gateway</h2>
<div class="builder-grid ledger-grid" id="worker-gateway"></div>
<div id="worker-gateway-error"></div>
<h2 class="section-title" data-i18n="mon.metadata">Runtime Metadata</h2>
<div class="builder-grid ledger-grid" id="runtime-metadata"></div>
<div id="runtime-metadata-error"></div>
<h2 class="section-title" data-i18n="mon.cache">Realtime Acceleration</h2>
<div class="builder-grid ledger-grid" id="cache-status"></div>
<div id="cache-status-error"></div>
<h2 class="section-title" data-i18n="mon.builders">Builders</h2>
<div class="builder-grid" id="builders"></div>
<div id="builders-empty"></div>
<h2 class="section-title" data-i18n="mon.instances">Cloud Instances</h2>
<div class="card">
  <div class="table-scroll"><table class="list" aria-label="Cloud instances">
    <thead><tr>
      <th data-i18n="th.instance">Instance</th><th data-i18n="th.provider">Provider</th>
      <th data-i18n="th.status">Status</th><th data-i18n="th.ip">IP</th>
      <th data-i18n="th.created">Created</th><th></th>
    </tr></thead>
    <tbody id="instances"></tbody>
  </table></div>
  <div id="instances-empty"></div>
</div>`

const monitorJS = `
async function load() {
  try {
    var ledgerResponse = await fetch('/api/ledger/status');
    if (ledgerResponse.status === 401) { location.href = '/login'; throw new Error('unauthorized'); }
    var ledger = await ledgerResponse.json();
    if (!ledgerResponse.ok && typeof ledger.ok === 'undefined') {
      throw new Error(ledger.details || ledger.error || ('HTTP ' + ledgerResponse.status));
    }
    var reconcile = ledger.last_reconcile || {};
    var ledgerGrid = document.getElementById('ledger');
    clear(ledgerGrid); clear(document.getElementById('ledger-error'));
    var ledgerCard = el('article', 'builder-card');
    ledgerCard.appendChild(el('h3', null, t('mon.ledger.shadow', 'PostgreSQL job authority')));
    var ledgerBadge = statusBadge(ledger.ok ? 'passed' : 'failed');
    ledgerBadge.lastChild.textContent = ledger.ok
      ? t('mon.ledger.ok', 'consistent')
      : t('mon.ledger.degraded', 'degraded');
    ledgerCard.appendChild(ledgerBadge);
    var ledgerMeta = el('div', 'meta');
    ledgerMeta.appendChild(el('span', null, t('mon.ledger.legacy', 'memory jobs ') + (reconcile.legacy_count || 0)));
    ledgerMeta.appendChild(el('span', null, t('mon.ledger.rows', 'ledger jobs ') + (reconcile.ledger_count || 0)));
    ledgerMeta.appendChild(el('span', null, t('mon.ledger.repaired', 'last repaired ') + (reconcile.repaired || 0)));
    ledgerMeta.appendChild(el('span', null, t('mon.ledger.errors', 'write errors ') + (ledger.write_errors || 0)));
    if (reconcile.checked_at) {
      ledgerMeta.appendChild(el('span', null, t('mon.ledger.checked', 'last checked ') + fmtTime(reconcile.checked_at)));
    }
    ledgerCard.appendChild(ledgerMeta);
    ledgerGrid.appendChild(ledgerCard);
  } catch (e) { showError('ledger-error', e); }
  try {
    var scheduler = await api('/api/scheduler/status');
    var schedulerGrid = document.getElementById('scheduler');
    clear(schedulerGrid); clear(document.getElementById('scheduler-error'));
    var schedulerCard = el('article', 'builder-card');
    schedulerCard.appendChild(el('h3', null, t('mon.scheduler.pg', 'PostgreSQL queue and leases')));
    var schedulerBadge = statusBadge(
      scheduler.healthy === false ||
      (scheduler.expired_leases || 0) > 0 ||
      (scheduler.unschedulable_tasks || 0) > 0 ? 'failed' : 'passed'
    );
    schedulerBadge.lastChild.textContent = scheduler.healthy === false
      ? t('mon.scheduler.degraded', 'degraded')
      : t('mon.scheduler.healthy', 'healthy');
    schedulerCard.appendChild(schedulerBadge);
    var schedulerMeta = el('div', 'meta');
    schedulerMeta.appendChild(el('span', null, t('mon.scheduler.queue', 'queued ') + (scheduler.queued_tasks || 0)));
    schedulerMeta.appendChild(el('span', null, t('mon.scheduler.unschedulable', 'capability mismatch ') + (scheduler.unschedulable_tasks || 0)));
    schedulerMeta.appendChild(el('span', null, t('mon.scheduler.running', 'running ') + (scheduler.running_tasks || 0)));
    schedulerMeta.appendChild(el('span', null, t('mon.scheduler.leases', 'active leases ') + (scheduler.active_leases || 0)));
    schedulerMeta.appendChild(el('span', null, t('mon.scheduler.expired', 'expired leases ') + (scheduler.expired_leases || 0)));
    schedulerMeta.appendChild(el('span', null, t('mon.scheduler.workers', 'active slots ') + (scheduler.active_workers || 0)));
    schedulerMeta.appendChild(el('span', null, t('mon.scheduler.capability', 'capability slots ') + (scheduler.capability_workers || 0)));
    schedulerMeta.appendChild(el('span', null, t('mon.scheduler.stale', 'stale slots ') + (scheduler.stale_workers || 0)));
    schedulerMeta.appendChild(el('span', null, t('mon.scheduler.attempts', 'attempts last hour ') + (scheduler.attempts_last_hour || 0)));
    var fairness = scheduler.fairness || {};
    schedulerMeta.appendChild(el('span', null,
      t('mon.scheduler.fair.projects', 'fair-queue projects ') +
      (fairness.eligible_projects || 0)));
    schedulerMeta.appendChild(el('span', null,
      t('mon.scheduler.fair.starved', 'anti-starvation boosted ') +
      (fairness.starved_projects || 0)));
    schedulerMeta.appendChild(el('span', null,
      t('mon.scheduler.fair.dispatches', 'fair dispatch admission/phase ') +
      (fairness.admission_dispatches || 0) + '/' +
      (fairness.phase_dispatches || 0)));
    schedulerMeta.appendChild(el('span', null,
      t('mon.scheduler.fair.maxwait', 'observed max wait ') +
      (fairness.max_queue_wait_seconds || 0) + 's'));
    var workerScoring = scheduler.worker_scoring || {};
    schedulerMeta.appendChild(el('span', null,
      t('mon.scheduler.score.decisions',
        'worker soft-score decisions/multi-candidate ') +
      (workerScoring.decisions_last_hour || 0) + '/' +
      (workerScoring.multi_candidate_last_hour || 0)));
    (Array.isArray(workerScoring.recent) ? workerScoring.recent : [])
      .slice(0, 6).forEach(function(decision) {
        schedulerMeta.appendChild(el('span', null,
          t('mon.scheduler.score.worker', 'worker decision ') +
          (decision.work_kind || 'unknown') +
          (decision.phase ? '/' + decision.phase : '') + ' · ' +
          (decision.worker || 'unknown') + ' · candidates ' +
          (decision.candidate_count || 0) + ' · pressure/failures ' +
          (decision.pressure_score || 0) + '/' +
          (decision.recent_failures || 0)));
      });
    var autoscaler = scheduler.autoscaler || {};
    schedulerMeta.appendChild(el('span', null,
      t('mon.scheduler.autoscale', 'autoscale recommendation ') +
      (autoscaler.mode || 'off') + ':' +
      (autoscaler.recommendation || 'off')));
    schedulerMeta.appendChild(el('span', null,
      t('mon.scheduler.autoscale.slots', 'slots active/desired ') +
      (autoscaler.active_slots || 0) + '/' +
      (autoscaler.desired_slots || 0)));
    schedulerMeta.appendChild(el('span', null,
      t('mon.scheduler.autoscale.demand', 'demand busy/backlog/unschedulable ') +
      (autoscaler.busy_slots || 0) + '/' +
      (autoscaler.backlog || 0) + '/' +
      (autoscaler.unschedulable_backlog || 0)));
    if (autoscaler.reason) {
      var autoscaleReason = autoscaler.reason ===
        'phase executor mode is shadow; capacity inventory only'
        ? t('mon.scheduler.autoscale.shadow',
            'phase executor mode is shadow; capacity inventory only')
        : autoscaler.reason;
      schedulerMeta.appendChild(el('span', null, autoscaleReason));
    }
    var capacityPools = Array.isArray(autoscaler.pools)
      ? autoscaler.pools : [];
    var blockedPools = capacityPools.filter(function(pool) {
      return (pool.unschedulable_backlog || 0) > 0;
    }).length;
    schedulerMeta.appendChild(el('span', null,
      t('mon.scheduler.autoscale.pools', 'capacity pools total/blocked ') +
      capacityPools.length + '/' + blockedPools));
    capacityPools.forEach(function(pool) {
      schedulerMeta.appendChild(el('span', null,
        t('mon.scheduler.autoscale.pool', 'capacity pool ') +
        (pool.provider || 'unknown') + '/' +
        (pool.execution_zone || 'default') + ' · ' +
        (pool.profile_id || pool.id || 'unknown') + ' · ' +
        (pool.recommendation || 'hold') + ' · ' +
        (pool.active_slots || 0) + '/' + (pool.desired_slots || 0) +
        ' active/desired · provider max ' +
        (pool.provider_max_slots || 0) + ' · ' + (pool.backlog || 0) + '/' +
        (pool.unschedulable_backlog || 0) + ' backlog/blocked'));
    });
    var actuator = autoscaler.actuator || {};
    schedulerMeta.appendChild(el('span', null,
      t('mon.scheduler.actuator', 'actuator actions open/failed ') +
      (actuator.open_actions || 0) + '/' +
      (actuator.failed_actions || 0)));
    schedulerMeta.appendChild(el('span', null,
      t('mon.scheduler.instances',
        'persistent instances provisioning/active/draining/deleting ') +
      (actuator.provisioning_instances || 0) + '/' +
      (actuator.active_instances || 0) + '/' +
      (actuator.draining_instances || 0) + '/' +
      (actuator.deleting_instances || 0)));
    (Array.isArray(actuator.actions) ? actuator.actions : [])
      .slice(0, 5).forEach(function(action) {
        var detail = t('mon.scheduler.action', 'capacity action ') +
          (action.kind || 'unknown') + ' · ' +
          (action.state || 'unknown') + ' · attempts ' +
          (action.attempts || 0) + ' · ' +
          (action.pool_id || 'unknown');
        if (action.failure_detail) detail += ' · ' + action.failure_detail;
        schedulerMeta.appendChild(el('span', null, detail));
      });
    (Array.isArray(actuator.instances) ? actuator.instances : [])
      .slice(0, 8).forEach(function(instance) {
        schedulerMeta.appendChild(el('span', null,
          t('mon.scheduler.instance', 'capacity instance ') +
          (instance.provider_instance_id || instance.id || 'unknown') +
          ' · ' + (instance.state || 'unknown') + ' · ' +
          (instance.pool_id || 'unknown')));
      });
    schedulerCard.appendChild(schedulerMeta);
    schedulerGrid.appendChild(schedulerCard);
    var history = scheduler.target_history || {};
    var targets = Array.isArray(history.targets) ? history.targets : [];
    var historyGrid = document.getElementById('target-history');
    var historyEmpty = document.getElementById('target-history-empty');
    clear(historyGrid); clear(historyEmpty);
    if (!targets.length) {
      historyEmpty.appendChild(el('div', 'empty', t('mon.targets.empty',
        'No terminal samples in the last 30 days')));
    }
    targets.forEach(function(target) {
      var card = el('article', 'builder-card target-card');
      card.appendChild(el('h3', null,
        (target.profile_id || 'compatibility') + ' · ' +
        (target.image_generation || 'unknown')));
      card.appendChild(el('div', 'ep',
        (target.project_name || target.project_id || 'unknown') + ' · ' +
        (target.provider || 'unknown') + '/' +
        (target.execution_zone || 'default') + ' · ' +
        (target.architecture || 'unknown') + ' · ' +
        (target.resource_class || 'default')));
      var meta = el('div', 'meta');
      (Array.isArray(target.windows) ? target.windows : [])
        .forEach(function(window) {
          var denominator = (window.successes || 0) +
            (window.failures || 0);
          var rate = denominator
            ? Number(window.success_rate_percent || 0).toFixed(1) + '%'
            : '—';
          var slo = window.insufficient_data
            ? t('mon.targets.insufficient', 'insufficient samples')
            : (window.slo_met ? 'SLO met' : 'SLO breach');
          meta.appendChild(el('span', null,
            (window.name || (window.hours + 'h')) + ' · ' +
            t('mon.targets.samples', 'samples success/failure/canceled ') +
            (window.samples || 0) + ' ' + (window.successes || 0) + '/' +
            (window.failures || 0) + '/' + (window.canceled || 0)));
          meta.appendChild(el('span', null,
            (window.name || '') + ' · ' +
            t('mon.targets.slo', 'success / SLO ') + rate + ' · ' + slo));
          meta.appendChild(el('span', null,
            (window.name || '') + ' · ' +
            t('mon.targets.latency', 'P50/P95 queue·run ') +
            (window.queue_p50_seconds || 0) + '/' +
            (window.queue_p95_seconds || 0) + 's · ' +
            (window.run_p50_seconds || 0) + '/' +
            (window.run_p95_seconds || 0) + 's'));
          meta.appendChild(el('span', null,
            (window.name || '') + ' · ' +
            t('mon.targets.cost', 'reserved/settled cost ') +
            ((window.reserved_cost_microunits || 0) / 1000000)
              .toFixed(3) + '/' +
            ((window.charged_cost_microunits || 0) / 1000000)
              .toFixed(3)));
          if (window.dominant_failure_class) {
            meta.appendChild(el('span', null,
              (window.name || '') + ' · ' +
              t('mon.targets.failure', 'dominant failure ') +
              window.dominant_failure_class));
          }
        });
      card.appendChild(meta);
      historyGrid.appendChild(card);
    });
  } catch (e) { showError('scheduler-error', e); }
  try {
    var gateway = await api('/api/worker-gateway/status');
    var gatewayGrid = document.getElementById('worker-gateway');
    clear(gatewayGrid); clear(document.getElementById('worker-gateway-error'));
    var gatewayCard = el('article', 'builder-card');
    gatewayCard.appendChild(el('h3', null, t('mon.gateway.mtls', 'Outbound pull and short-lived mTLS identity')));
    var gatewayBadge = statusBadge(
      gateway.enabled && gateway.issuer_healthy !== false ? 'passed' :
      (gateway.enabled ? 'failed' : 'pending')
    );
    gatewayBadge.lastChild.textContent = gateway.enabled
      ? t('mon.gateway.enabled', 'enabled')
      : t('mon.gateway.disabled', 'compatibility mode');
    gatewayCard.appendChild(gatewayBadge);
    var gatewayMeta = el('div', 'meta');
    gatewayMeta.appendChild(el('span', null, t('mon.gateway.authority', 'authority ') + (gateway.authority || 'memory')));
    gatewayMeta.appendChild(el('span', null, t('mon.gateway.connected', 'connected ') + (gateway.connected_sessions || 0)));
    gatewayMeta.appendChild(el('span', null, t('mon.gateway.registered', 'registered ') + (gateway.registered_sessions || 0)));
    gatewayMeta.appendChild(el('span', null, t('mon.gateway.tasks', 'pending tasks ') + (gateway.pending_tasks || 0)));
    gatewayMeta.appendChild(el('span', null, t('mon.gateway.uploads', 'pending uploads ') + (gateway.pending_uploads || 0)));
    gatewayMeta.appendChild(el('span', null, t('mon.gateway.issuers', 'issuer generations ') +
      (gateway.active_issuers || 0) + ' active / ' +
      (gateway.draining_issuers || 0) + ' draining / ' +
      (gateway.revoked_issuers || 0) + ' revoked'));
    gatewayMeta.appendChild(el('span', null, t('mon.gateway.certs', 'workload certificates ') +
      (gateway.active_certificates || 0) + ' active / ' +
      (gateway.revoked_certificates || 0) + ' revoked'));
    if (gateway.expiring_certificates) {
      gatewayMeta.appendChild(el('span', null, t('mon.gateway.expiring', 'expiring within 30m ') +
        gateway.expiring_certificates));
    }
    gatewayMeta.appendChild(el('span', null, t('mon.gateway.provider', 'issuer provider ') +
      (gateway.issuer_provider || 'unknown') + ':' + (gateway.issuer_id || 'unknown')));
    var issuerRuntime = gateway.issuer_runtime || {};
    gatewayMeta.appendChild(el('span', null, t('mon.gateway.provider.health', 'provider health ') +
      (issuerRuntime.healthy ? 'healthy' : 'unhealthy') +
      ' / failures ' + (issuerRuntime.consecutive_failures || 0)));
    if (issuerRuntime.last_success_at) {
      gatewayMeta.appendChild(el('span', null, t('mon.gateway.provider.success', 'last issuer success ') +
        fmtTime(issuerRuntime.last_success_at)));
    }
    if (issuerRuntime.last_failure_at) {
      gatewayMeta.appendChild(el('span', null, t('mon.gateway.provider.failure', 'last issuer failure ') +
        fmtTime(issuerRuntime.last_failure_at)));
    }
    if (issuerRuntime.last_error) {
      gatewayMeta.appendChild(el('span', null, t('mon.gateway.provider.error', 'issuer error ') +
        issuerRuntime.last_error));
    }
    gatewayMeta.appendChild(el('span', null, t('mon.gateway.inbound', 'inbound builder API ') + (gateway.inbound_builder_api ? 'on' : 'off')));
    gatewayMeta.appendChild(el('span', null, t('mon.gateway.protocol', 'executor protocol ') + 'v' + (gateway.executor_protocol || 0)));
    gatewayMeta.appendChild(el('span', null, t('mon.gateway.phase', 'phase executor ') + (gateway.phase_executor_mode || 'shadow')));
    var phaseWork = gateway.phase_work || {};
    gatewayMeta.appendChild(el('span', null, t('mon.gateway.phase.active', 'active phase work ') + (phaseWork.active || 0)));
    gatewayMeta.appendChild(el('span', null, t('mon.gateway.phase.claimed', 'claimed ') + (phaseWork.claimed || 0)));
    gatewayMeta.appendChild(el('span', null, t('mon.gateway.phase.ready', 'ready ') + (phaseWork.ready || 0)));
    gatewayMeta.appendChild(el('span', null, t('mon.scheduler.unschedulable', 'capability mismatch ') + (phaseWork.unschedulable || 0)));
    gatewayMeta.appendChild(el('span', null, t('mon.gateway.phase.blocked', 'blocked ') + (phaseWork.blocked || 0)));
    if (phaseWork.failed) gatewayMeta.appendChild(el('span', null, t('mon.gateway.phase.failed', 'failed ') + phaseWork.failed));
    gatewayMeta.appendChild(el('span', null, t('mon.gateway.ttl', 'certificate TTL ') + (gateway.certificate_ttl_min || 0) + 'm'));
    gatewayCard.appendChild(gatewayMeta);
    gatewayGrid.appendChild(gatewayCard);
    try {
      var identityInventory = await api('/api/worker-gateway/identities');
      var issuerDetails = el('details', 'factory-step-details');
      issuerDetails.appendChild(el('summary', null,
        t('mon.gateway.inventory', 'Issuer and certificate inventory')));
      var inventoryBody = el('div', 'meta');
      (identityInventory.issuers || []).forEach(function (issuer) {
        inventoryBody.appendChild(el('span', 'mono',
          (issuer.fingerprint || '').slice(0, 12) + '… ' +
          (issuer.state || 'unknown') + ' · ' +
          (issuer.provider || 'unknown') + ':' + (issuer.issuer_id || 'unknown') +
          ' · active leaves ' + (issuer.active_certificates || 0) +
          ' · expires ' + fmtTime(issuer.not_after)));
      });
      if (!(identityInventory.issuers || []).length) {
        inventoryBody.appendChild(el('span', null,
          t('mon.gateway.inventory.empty', 'No certificate has been issued yet.')));
      }
      inventoryBody.appendChild(el('span', null,
        t('mon.gateway.inventory.recent', 'recent certificates ') +
        (identityInventory.certificates || []).length + ' / ' +
        (identityInventory.certificate_limit || 100)));
      issuerDetails.appendChild(inventoryBody);
      gatewayCard.appendChild(issuerDetails);
    } catch (inventoryError) {
      gatewayMeta.appendChild(el('span', null,
        t('mon.gateway.inventory.error', 'identity inventory unavailable')));
    }
  } catch (e) { showError('worker-gateway-error', e); }
  try {
    var metadataResponse = await api('/api/runtime-metadata/status');
    var metadata = metadataResponse.status || {};
    var metadataGrid = document.getElementById('runtime-metadata');
    clear(metadataGrid); clear(document.getElementById('runtime-metadata-error'));
    var metadataCard = el('article', 'builder-card');
    metadataCard.appendChild(el('h3', null, t('mon.metadata.db', 'Infra, artifacts and image factory')));
    var artifactIntegrityOK = (metadata.missing_artifacts || 0) === 0 && (metadata.corrupt_artifacts || 0) === 0;
    metadataCard.appendChild(statusBadge(metadataResponse.ok && artifactIntegrityOK ? 'passed' : 'failed'));
    var metadataMeta = el('div', 'meta');
    metadataMeta.appendChild(el('span', null, t('mon.metadata.infra', 'live infra ') + (metadata.live_infra || 0)));
    metadataMeta.appendChild(el('span', null, t('mon.metadata.cleanup', 'cleanup failed ') + (metadata.cleanup_failed_infra || 0)));
    metadataMeta.appendChild(el('span', null, t('mon.metadata.published', 'published artifacts ') + (metadata.published_artifacts || 0)));
    metadataMeta.appendChild(el('span', null, t('mon.metadata.staged', 'staged artifacts ') + (metadata.staged_artifacts || 0)));
    metadataMeta.appendChild(el('span', null, t('mon.metadata.missing', 'missing artifacts ') + (metadata.missing_artifacts || 0)));
    metadataMeta.appendChild(el('span', null, t('mon.metadata.corrupt', 'corrupt artifacts ') + (metadata.corrupt_artifacts || 0)));
    metadataMeta.appendChild(el('span', null, t('mon.metadata.orphaned', 'orphaned artifacts ') + (metadata.orphaned_artifacts || 0)));
    metadataMeta.appendChild(el('span', null, t('mon.metadata.factory', 'factory runs ') + (metadata.factory_runs || 0)));
    if (metadata.last_metadata_update_at) metadataMeta.appendChild(el('span', null, fmtTime(metadata.last_metadata_update_at)));
    metadataCard.appendChild(metadataMeta);
    metadataGrid.appendChild(metadataCard);
  } catch (e) { showError('runtime-metadata-error', e); }
  try {
    var cacheStatus = await api('/api/cache/status');
    var cacheGrid = document.getElementById('cache-status');
    clear(cacheGrid); clear(document.getElementById('cache-status-error'));
    var cacheCard = el('article', 'builder-card');
    cacheCard.appendChild(el('h3', null, t('mon.cache.redis', 'Redis presence, rate limits and events')));
    cacheCard.appendChild(statusBadge(cacheStatus.ok ? 'passed' : 'failed'));
    var cacheMeta = el('div', 'meta');
    cacheMeta.appendChild(el('span', null, t('mon.cache.presence', 'control-plane instances ') + (cacheStatus.control_plane_presence || 0)));
    cacheMeta.appendChild(el('span', null, t('mon.cache.fallback', 'PostgreSQL polling fallback')));
    if (cacheStatus.last_success_at) cacheMeta.appendChild(el('span', null, fmtTime(cacheStatus.last_success_at)));
    cacheCard.appendChild(cacheMeta);
    cacheGrid.appendChild(cacheCard);
  } catch (e) { showError('cache-status-error', e); }
  try {
    var data = await api('/api/builders/status');
    var builders = (data && data.builders) || [];
    var grid = document.getElementById('builders');
    var emptyBox = document.getElementById('builders-empty');
    clear(grid); clear(emptyBox);
    if (!builders.length) {
      emptyBox.appendChild(el('div', 'empty', t('mon.noBuilders', 'No registered builders. Static builders register automatically once SERVER_URL is set; ephemeral cloud instances are not listed here.')));
    }
    builders.forEach(function (b) {
      var c = el('article', 'builder-card');
      c.appendChild(el('h3', null, b.id || '-'));
      c.appendChild(el('p', 'ep mono', b.endpoint || ''));
      c.appendChild(statusBadge(b.status));
      var meta = el('div', 'meta');
      meta.appendChild(el('span', null, t('mon.archLabel', 'arch ') + (b.architecture || '-')));
      meta.appendChild(el('span', null, t('mon.loadLabel', 'load ') + (b.current_load || 0) + '/' + (b.capacity || 0)));
      if (b.native_job_policy) {
        meta.appendChild(el('span', null, t('mon.policyLabel', 'isolation ') + b.native_job_policy));
      }
      meta.appendChild(el('span', null, b.accepting_builds === false
        ? t('mon.notAccepting', 'draining; no new jobs')
        : t('mon.accepting', 'accepting jobs')));
      c.appendChild(meta);
      grid.appendChild(c);
    });
  } catch (e) { showError('builders-empty', e); }
  try {
    var r = await api('/api/instances');
    var list = Array.isArray(r) ? r : (r.instances || []);
    var tb = document.getElementById('instances');
    var emptyBox = document.getElementById('instances-empty');
    clear(tb); clear(emptyBox);
    if (!list.length) { emptyBox.appendChild(el('div', 'empty', t('mon.noInstances', 'No cloud instances running.'))); return; }
    list.forEach(function (i) {
      var tr = el('tr');
      tr.appendChild(el('td', 'mono', i.id || '-'));
      tr.appendChild(el('td', 'sec', i.provider || '-'));
      var st = el('td'); st.appendChild(statusBadge(i.status)); tr.appendChild(st);
      tr.appendChild(el('td', 'mono sec', i.ip_address || i.public_ip || '-'));
      tr.appendChild(el('td', 'sec', fmtTime(i.created_at)));
      var act = el('td');
      var sh = el('a', 'btn', t('mon.shell', 'Shell'));
      sh.href = '/shell/' + encodeURIComponent(i.id);
      act.appendChild(sh);
      tr.appendChild(act);
      tb.appendChild(tr);
    });
  } catch (e) { showError('instances-empty', e); }
}
function onLangChange() { load(); }
document.getElementById('refresh').addEventListener('click', load);
load();
setInterval(load, 15000);
`

// ---------------------------------------------------------------------------
// Settings
// ---------------------------------------------------------------------------

const settingsContent = `
<div class="page-head">
  <div><h1 data-i18n="set.h1">Settings</h1><p class="sub" data-i18n="set.sub">Cloud build configuration — saved changes apply immediately and override server.conf</p></div>
</div>
<div class="settings-layout">
<nav class="subnav" id="subnav" aria-label="Settings sections">
  <span class="subnav-label" data-i18n="set.cat.general">General</span>
  <a data-sec="general" class="active" data-i18n="set.sec.general">Backend &amp; Test</a>
  <span class="subnav-label" data-i18n="set.cat.infra">Infrastructure</span>
  <a data-sec="pve">Proxmox VE</a>
  <a data-sec="gcp">Google Cloud</a>
  <a data-sec="aws">AWS</a>
  <a data-sec="builders" data-i18n="set.sec.builders">Static Builders</a>
  <a data-sec="mirrors" data-i18n="set.sec.mirrors">Mirrors</a>
  <a data-sec="buildconf" data-i18n="set.sec.buildconf">Build Config</a>
  <span class="subnav-label" data-i18n="set.cat.access">Access</span>
  <a data-sec="ssh" data-i18n="set.sec.ssh">SSH Keys</a>
  <a data-sec="gpg" data-i18n="set.sec.gpg">GPG Signing</a>
  <a data-sec="net" data-i18n="set.sec.net">Network &amp; Delivery</a>
  <a data-sec="security">Sessions &amp; Security</a>
</nav>
<div class="settings-panels">
<form id="settings-form">

<section class="panel" data-panel="security">
  <div class="card"><h3 class="card-title">Identity providers</h3><div class="card-pad">
    <p class="hint">Upstream credentials are exchanged once. Ordinary API requests use only a short-lived Portage Engine session.</p>
    <div id="identity-provider-status" class="provider-status-list"></div>
  </div></div>
  <div class="card"><h3 class="card-title">Federated sessions</h3><div class="card-pad">
    <p class="hint">Only token hashes and lifecycle metadata are retained. Revocation is enforced by PostgreSQL across all control-plane replicas.</p>
    <div class="table-scroll"><table class="list" aria-label="Federated sessions">
      <thead><tr><th>Session</th><th>Issued</th><th>Last seen</th><th>Expires</th><th>Auth context</th><th>Status</th><th></th></tr></thead>
      <tbody id="session-rows"></tbody>
    </table></div>
    <div id="session-empty"></div>
    <div class="form-actions">
      <button class="btn" type="button" id="refresh-sessions">Refresh</button>
      <button class="btn danger" type="button" id="revoke-all-sessions">Revoke all sessions</button>
      <a class="btn" href="/login?step_up=1">Re-authenticate with identity provider</a>
    </div>
  </div></div>
</section>

<section class="panel active" data-panel="general">
  <div class="card"><h3 class="card-title" data-i18n="set.backend">Build Backend</h3><div class="card-pad form-grid">
    <div class="field">
      <label for="provider" data-i18n="set.provider">Default provider</label>
      <select id="provider">
        <option value="pve">Proxmox VE</option>
        <option value="gcp">Google Cloud</option>
        <option value="aws">AWS (beta)</option>
      </select>
      <p class="hint" data-i18n="set.provider.hint">Used when no static builders are configured; more backends can be added over time</p>
    </div>
    <div class="field">
      <label for="ttl" data-i18n="set.ttl">Idle instance TTL (minutes)</label>
      <input type="number" id="ttl" min="0">
      <p class="hint" data-i18n="set.ttl.hint">Maximum instance lifetime and orphan cleanup window; successful native builders are destroyed immediately and never reused</p>
    </div>
  </div>
  <div class="card-pad" style="padding-top:0">
    <div class="field check">
      <input type="checkbox" id="verify_install" checked disabled>
      <label for="verify_install" data-i18n="set.verify">Every binpkg must install from its job quarantine before publication</label>
    </div>
  </div></div>

  <div class="card"><h3 class="card-title" data-i18n="set.testbuild">Test Build</h3><div class="card-pad">
    <p class="hint" style="margin-bottom:10px" data-i18n="set.testbuild.hint">Runs the full pipeline with current settings: provision a builder, emerge the package, collect the artifact into the binhost.</p>
    <div class="form-grid">
      <div class="field">
        <label for="test_pkg" data-i18n="set.testbuild.pkg">Package atom</label>
        <input type="text" id="test_pkg" value="app-misc/jq">
      </div>
    </div>
    <div class="form-actions">
      <button class="btn blue" type="button" id="test-build" data-i18n="set.testbuild.go">Start Test Build</button>
      <span class="save-msg" id="test-build-msg" role="status"></span>
    </div>
    <div id="test-build-result" style="margin-top:10px"></div>
  </div></div>
</section>

<section class="panel" data-panel="pve">
  <div class="card"><h3 class="card-title" data-i18n="set.conn">Connection</h3><div class="card-pad">
    <div class="form-grid">
      <div class="field">
        <label for="pve_endpoint" data-i18n="set.endpoint">API endpoint</label>
        <input type="text" id="pve_endpoint" placeholder="https://pve.example.com:8006">
      </div>
      <div class="field">
        <label for="pve_token_id" data-i18n="set.tokenid">API token ID</label>
        <input type="text" id="pve_token_id" placeholder="root@pam!terraform">
      </div>
      <div class="field">
        <label for="pve_token_secret" data-i18n="set.secret">API token secret</label>
        <input type="password" id="pve_token_secret" autocomplete="off">
        <p class="hint" id="secret-hint"></p>
      </div>
      <div class="field">
        <label for="pve_username" data-i18n="set.pveuser">Username (alternative to token)</label>
        <input type="text" id="pve_username" placeholder="terraform-prov@pve" autocomplete="off">
        <p class="hint" data-i18n="set.pveuser.hint">user@realm format; a token is preferred when both are set</p>
      </div>
      <div class="field">
        <label for="pve_password" data-i18n="set.pvepass">Password</label>
        <input type="password" id="pve_password" autocomplete="off">
        <p class="hint" id="pve-pass-hint"></p>
      </div>
    </div>
    <div class="field check">
      <input type="checkbox" id="pve_insecure">
      <label for="pve_insecure" data-i18n="set.tls">Skip TLS verification (self-signed cert; adding the PVE CA to the system trust store is safer)</label>
    </div>
  </div>
  <div class="card-actions">
    <button class="btn" type="button" id="test" data-i18n="set.test">Test Connection</button>
    <span class="save-msg" id="test-msg" role="status"></span>
  </div></div>

  <div class="card" id="test-card" style="display:none">
    <h3 class="card-title" data-i18n="set.clusternodes">Cluster Nodes</h3>
    <div class="table-scroll"><table class="list" aria-label="Cluster nodes">
      <thead><tr>
        <th data-i18n="th.node">Node</th><th data-i18n="th.status">Status</th>
        <th data-i18n="th.freemem">Free Memory</th><th data-i18n="th.cpu">CPU Load</th>
        <th data-i18n="th.hastpl">Has Template</th>
      </tr></thead>
      <tbody id="test-rows"></tbody>
    </table></div>
  </div>

  <div class="card"><h3 class="card-title" data-i18n="set.placement">Node Scheduling</h3><div class="card-pad">
    <div class="radio-row">
      <label><input type="radio" name="placement" id="place_auto" checked>
        <span data-i18n="set.place.auto">Automatic (recommended)</span></label>
      <label><input type="radio" name="placement" id="place_manual">
        <span data-i18n="set.place.manual">Pin to a node</span></label>
    </div>
    <p class="hint" style="margin-bottom:10px" data-i18n="set.place.auto.hint">Each build queries live cluster load and lands on the least-loaded eligible node</p>
    <div class="form-grid">
      <div class="field" id="manual-node-field" style="display:none">
        <label for="pve_node_manual" data-i18n="set.node">Target node</label>
        <input type="text" id="pve_node_manual" placeholder="pve">
      </div>
      <div class="field" id="candidate-field">
        <label for="pve_nodes" data-i18n="set.nodes">Candidate nodes (optional, comma-separated)</label>
        <input type="text" id="pve_nodes" placeholder="pve1,pve2,pve3">
        <p class="hint" data-i18n="set.nodes.hint">Use on shared-storage clusters; empty restricts to template-hosting nodes</p>
      </div>
    </div>
  </div></div>

  <div class="card"><h3 class="card-title" data-i18n="set.resources">Resources</h3><div class="card-pad form-grid">
    <div class="field">
      <label for="pve_template" data-i18n="set.template">VM template</label>
      <input type="text" id="pve_template" placeholder="gentoo-native-cloudinit-template">
      <p class="hint" data-i18n="set.template.hint">Must be a QEMU template with cloud-init and qemu-guest-agent installed</p>
    </div>
    <div class="field">
      <label for="pve_storage" data-i18n="set.storage">Storage pool</label>
      <input type="text" id="pve_storage" placeholder="local-lvm">
    </div>
    <div class="field">
      <label for="pve_network" data-i18n="set.bridge">Network bridge</label>
      <input type="text" id="pve_network" placeholder="vmbr0">
    </div>
    <div class="field">
      <label for="pve_nameserver" data-i18n="set.nameserver">DNS server for build VMs (optional)</label>
      <input type="text" id="pve_nameserver" placeholder="10.0.0.252">
      <p class="hint" data-i18n="set.nameserver.hint">Pushed via cloud-init; set your internal DNS so mirror/registry domains resolve on build VMs</p>
    </div>
    <div class="field">
      <label for="pve_cicustom" data-i18n="set.cicustom">cloud-init snippet (cicustom, optional)</label>
      <input type="text" id="pve_cicustom" placeholder="vendor=local:snippets/vendor.yaml">
      <p class="hint" data-i18n="set.cicustom.hint">Preserved on cloned VMs; use a vendor snippet that installs qemu-guest-agent when the base image lacks it</p>
    </div>
  </div></div>
</section>

<section class="panel" data-panel="gcp">
  <div class="card"><h3 class="card-title">Google Cloud</h3><div class="card-pad form-grid">
    <div class="field">
      <label for="gcp_project" data-i18n="set.gcp.project">Project</label>
      <input type="text" id="gcp_project" placeholder="my-project">
    </div>
    <div class="field">
      <label for="gcp_region" data-i18n="set.gcp.region">Region</label>
      <input type="text" id="gcp_region" placeholder="us-central1">
    </div>
    <div class="field">
      <label for="gcp_zone" data-i18n="set.gcp.zone">Zone</label>
      <input type="text" id="gcp_zone" placeholder="us-central1-a">
    </div>
    <div class="field">
      <label for="gcp_key_file" data-i18n="set.gcp.keyfile">Service account key file (path on server)</label>
      <input type="text" id="gcp_key_file" placeholder="/var/lib/portage-engine/gcp-key.json">
    </div>
  </div></div>
</section>

<section class="panel" data-panel="aws">
  <div class="card"><h3 class="card-title">AWS</h3><div class="card-pad form-grid">
    <div class="field">
      <label for="aws_region" data-i18n="set.gcp.region">Region</label>
      <input type="text" id="aws_region" placeholder="us-east-1">
    </div>
    <div class="field">
      <label for="aws_zone" data-i18n="set.gcp.zone">Zone</label>
      <input type="text" id="aws_zone" placeholder="us-east-1a">
    </div>
    <div class="field">
      <label for="aws_access_key" data-i18n="set.aws.ak">Access key ID</label>
      <input type="text" id="aws_access_key" autocomplete="off">
    </div>
    <div class="field">
      <label for="aws_secret_key" data-i18n="set.aws.sk">Secret access key</label>
      <input type="password" id="aws_secret_key" autocomplete="off">
      <p class="hint" id="aws-secret-hint"></p>
    </div>
  </div></div>
</section>

<section class="panel" data-panel="builders">
  <div class="card"><h3 class="card-title" data-i18n="set.sec.builders">Static Builders</h3><div class="card-pad">
    <div class="field">
      <label for="remote_builders" data-i18n="set.builders">Builder URLs (comma-separated)</label>
      <input type="text" id="remote_builders" placeholder="http://builder1:9090,http://builder2:9090">
      <p class="hint" data-i18n="set.builders.hint">When set, builds are dispatched round-robin to these builders; when empty, an ephemeral cloud VM is provisioned per build</p>
    </div>
  </div></div>
</section>

<section class="panel" data-panel="mirrors">
  <div class="card"><h3 class="card-title" data-i18n="set.sec.mirrors">Mirrors</h3><div class="card-pad">
    <p class="hint" style="margin-bottom:10px" data-i18n="set.mirrors.hint">Internal mirrors used when bootstrapping build instances — dramatically faster deploys on a LAN. All optional.</p>
    <div class="form-grid">
      <div class="field">
        <label for="gentoo_mirror" data-i18n="set.mirrors.gentoo">Gentoo mirror (GENTOO_MIRRORS)</label>
        <input type="text" id="gentoo_mirror" placeholder="http://10.31.0.2/gentoo">
        <p class="hint" data-i18n="set.mirrors.gentoo.hint">Distfiles and webrsync snapshots; lands in make.conf on build instances</p>
      </div>
      <div class="field">
        <label for="portage_sync_method" data-i18n="set.mirrors.method">Portage tree sync method</label>
        <select id="portage_sync_method">
          <option value="webrsync">webrsync (snapshot, recommended)</option>
          <option value="rsync">rsync (incremental, needs sync URI)</option>
        </select>
        <p class="hint" data-i18n="set.mirrors.method.hint">webrsync downloads one snapshot tarball from the Gentoo mirror; rsync syncs file-by-file and is slow without a LAN rsync mirror</p>
      </div>
      <div class="field">
        <label for="portage_sync_uri" data-i18n="set.mirrors.sync">Portage sync URI (optional)</label>
        <input type="text" id="portage_sync_uri" placeholder="rsync://mirror/gentoo-portage">
        <p class="hint" data-i18n="set.mirrors.sync.hint">Custom repos.conf sync-uri; empty uses webrsync snapshots from the Gentoo mirror</p>
      </div>
    </div>
  </div></div>
  <div class="card"><h3 class="card-title" data-i18n="set.sec.upload">Artifact Upload</h3><div class="card-pad">
    <p class="hint" data-i18n="set.upload.desc">When set, only centrally promoted binpkgs that passed quarantined install verification are pushed to the internal mirror with the Packages index and signing pubkey.</p>
    <div class="grid-2">
      <div class="field">
        <label for="upload_url" data-i18n="set.upload.url">Mirror base URL</label>
        <input type="text" id="upload_url" placeholder="http://10.31.0.2">
        <p class="hint" data-i18n="set.upload.url.hint">Empty disables uploading; packages then serve only from this server's /binpkgs</p>
      </div>
      <div class="field">
        <label for="upload_dir" data-i18n="set.upload.dir">Artifact directory</label>
        <input type="text" id="upload_dir" placeholder="portage-engine">
        <p class="hint" data-i18n="set.upload.dir.hint">Files land under /local/&lt;dir&gt;/… — that URL becomes the LAN binhost</p>
      </div>
      <div class="field">
        <label for="upload_user" data-i18n="set.upload.user">Username</label>
        <input type="text" id="upload_user" autocomplete="off">
      </div>
      <div class="field">
        <label for="upload_password" data-i18n="set.upload.pass">Password</label>
        <input type="password" id="upload_password" autocomplete="new-password">
        <p class="hint" id="upload-pass-hint"></p>
      </div>
    </div>
  </div></div>
</section>

<section class="panel" data-panel="buildconf">
  <div class="card"><h3 class="card-title" data-i18n="set.sec.buildconf">Build Config</h3><div class="card-pad">
    <div class="field">
      <label for="make_conf_extra" data-i18n="set.makeconf">Extra make.conf content</label>
      <textarea id="make_conf_extra" spellcheck="false" placeholder="USE=&quot;-doc -test&quot;&#10;ACCEPT_LICENSE=&quot;*&quot;&#10;FEATURES=&quot;parallel-fetch&quot;"></textarea>
      <p class="hint" data-i18n="set.makeconf.hint">Appended verbatim to the generated make.conf on every build instance (global USE, ACCEPT_LICENSE, FEATURES, EMERGE_DEFAULT_OPTS, ...). Per-package USE comes from the client's config bundle.</p>
    </div>
    <div class="field">
      <label for="build_features" data-i18n="set.buildfeatures">Native build FEATURES</label>
      <input type="text" id="build_features" spellcheck="false" placeholder="parallel-fetch">
      <p class="hint" data-i18n="set.buildfeatures.hint">Appended to the disposable native Gentoo root's FEATURES; leave empty to use the image/profile defaults.</p>
    </div>
    <div class="field">
      <label data-i18n="set.buildmode">Build mode</label>
      <p>Native Gentoo disposable root / VM</p>
      <p class="hint" data-i18n="set.buildmode.hint">The build backend is fixed to a disposable native Gentoo root/VM; Docker builders have been removed.</p>
    </div>
  </div></div>
</section>

<section class="panel" data-panel="ssh">
  <div class="card"><h3 class="card-title" data-i18n="set.sec.ssh">SSH Keys</h3><div class="card-pad">
    <div class="form-grid">
      <div class="field">
        <label for="ssh_key_path" data-i18n="set.keypath">Private key path</label>
        <input type="text" id="ssh_key_path" placeholder="/var/lib/portage-engine/id_ed25519">
        <p class="hint" data-i18n="set.keypath.hint">The matching .pub is injected into new VMs via cloud-init</p>
      </div>
      <div class="field">
        <label for="ssh_user" data-i18n="set.sshuser">SSH user</label>
        <input type="text" id="ssh_user" placeholder="root">
        <p class="hint" data-i18n="set.sshuser.hint">The deployment script requires root</p>
      </div>
      <div class="field">
        <label for="ssh_known_hosts" data-i18n="set.knownhosts">known_hosts path (optional)</label>
        <input type="text" id="ssh_known_hosts">
      </div>
    </div>
    <div class="field check">
      <input type="checkbox" id="ssh_insecure">
      <label for="ssh_insecure" data-i18n="set.hostkey">Skip SSH host key verification (needed for first connect to fresh VMs, or use known_hosts above)</label>
    </div>
  </div></div>
</section>

<section class="panel" data-panel="gpg">
  <div class="card"><h3 class="card-title" data-i18n="set.sec.gpg">GPG Signing</h3><div class="card-pad">
    <div class="stat-grid" style="margin-bottom:16px">
      <div class="stat-tile">
        <h4 data-i18n="set.gpg.state">Signing</h4>
        <div class="num" id="gpg-state" style="font: var(--title-3-emphasized)">…</div>
      </div>
      <div class="stat-tile">
        <h4 data-i18n="set.gpg.keyid">Key ID</h4>
        <div class="num wrap" id="gpg-keyid">-</div>
      </div>
      <div class="stat-tile">
        <h4 data-i18n="set.gpg.mode">Isolation mode</h4>
        <div class="num wrap" id="gpg-mode">-</div>
      </div>
      <div class="stat-tile">
        <h4 data-i18n="set.gpg.queue">Signing queue</h4>
        <div class="num wrap" id="gpg-queue">-</div>
      </div>
    </div>
    <p class="hint" data-i18n="set.gpg.hint">The private key exists only in the isolated portage-signer key volume. The control plane submits digest-bound tasks through PostgreSQL; neither builders nor this WebUI can read or generate private keys.</p>
  </div></div>
  <div class="card"><h3 class="card-title" data-i18n="set.gpg.pubkey">Public Key</h3><div class="card-pad">
    <pre class="log-view" id="gpg-pubkey" style="max-height:260px">-</pre>
    <div class="form-actions">
      <a class="btn" href="/api/keys/download" data-i18n="set.gpg.download">Download Public Key</a>
    </div>
  </div></div>
</section>

<section class="panel" data-panel="net">
  <div class="card"><h3 class="card-title" data-i18n="set.sec.net">Network &amp; Delivery</h3><div class="card-pad form-grid">
    <div class="field">
      <label for="callback" data-i18n="set.callback">Callback URL</label>
      <input type="text" id="callback" placeholder="http://10.0.0.10:8080">
      <p class="hint" data-i18n="set.callback.hint">Required by the publication gate: builders use this reachable URL to read the job quarantine binhost</p>
    </div>
    <div class="field">
      <label for="bin_path" data-i18n="set.binpath">Builder binary path</label>
      <input type="text" id="bin_path" placeholder="bin/portage-builder-linux-amd64">
      <p class="hint" data-i18n="set.binpath.hint">Copied to instances via scp; must be linux and arch-matching</p>
    </div>
    <div class="field">
      <label for="bin_url" data-i18n="set.binurl">Builder binary URL (optional)</label>
      <input type="text" id="bin_url" placeholder="https://example.com/portage-builder-linux-amd64">
      <p class="hint" data-i18n="set.binurl.hint">Downloaded by the instance at bootstrap; path wins if both are set</p>
    </div>
    <div class="field">
      <label for="bin_sha256">Builder binary SHA-256</label>
      <input type="text" id="bin_sha256" maxlength="64" placeholder="64 lowercase hexadecimal characters">
      <p class="hint">Required when using a URL; the instance verifies it before installing or running the binary</p>
    </div>
  </div></div>
</section>

<div class="settings-footer">
  <button class="btn blue" type="submit" id="save" data-i18n="set.save">Save</button>
  <span class="save-msg" id="msg" role="status"></span>
</div>
</form>
</div>
</div>`

const settingsJS = `
var form = document.getElementById('settings-form');
var msg = document.getElementById('msg');
var lastSettings = null;

async function loadSessions() {
  var rows = document.getElementById('session-rows');
  var empty = document.getElementById('session-empty');
  if (!rows || !empty) return;
  clear(rows); clear(empty);
  try {
    var result = await api('/api/iam/sessions');
    var sessions = Array.isArray(result.sessions) ? result.sessions : [];
    if (!sessions.length) {
      empty.appendChild(el('div', 'empty', 'No OIDC sessions are registered.'));
      return;
    }
    sessions.forEach(function (session) {
      var tr = el('tr');
      tr.appendChild(el('td', 'mono sec', session.id.slice(0, 8) +
        (session.id === result.current_session_id ? ' · current' : '')));
      tr.appendChild(el('td', 'sec', fmtTime(session.issued_at)));
      tr.appendChild(el('td', 'sec', fmtTime(session.last_seen_at)));
      tr.appendChild(el('td', 'sec', fmtTime(session.expires_at)));
      tr.appendChild(el('td', 'sec', (session.acr || '-') + ' · ' +
        ((session.amr || []).join(', ') || '-')));
      tr.appendChild(el('td', 'sec', session.revoked_at ? 'revoked' : 'active'));
      var action = el('td');
      if (!session.revoked_at) {
        var revoke = el('button', 'btn', 'Revoke');
        revoke.type = 'button';
        revoke.addEventListener('click', async function () {
          if (!confirm('Revoke this session?')) return;
          await api('/api/iam/sessions?session_id=' +
            encodeURIComponent(session.id), {method: 'DELETE'});
          if (session.id === result.current_session_id) location.href = '/logout';
          else loadSessions();
        });
        action.appendChild(revoke);
      }
      tr.appendChild(action);
      rows.appendChild(tr);
    });
  } catch (error) {
    empty.appendChild(el('div', 'empty', 'Sessions unavailable: ' + error.message));
  }
}
function renderIdentityProviders() {
  var target = document.getElementById('identity-provider-status');
  if (!target) return;
  clear(target);
  var providers = window.peIAM && Array.isArray(window.peIAM.identityProviders)
    ? window.peIAM.identityProviders : [];
  if (!providers.length) {
    target.appendChild(el('div', 'empty', 'No federated provider is active in the current authentication mode.'));
    return;
  }
  for (var i = 0; i < providers.length; i++) {
    var provider = providers[i];
    var text = provider.display_name + ' · ' + provider.type;
    if (provider.backchannel_logout_enabled) text += ' · back-channel logout';
    target.appendChild(el('div', 'provider-status-row', text));
  }
}

/* --- sub-navigation --- */
function showSection(sec) {
  var links = document.querySelectorAll('#subnav a[data-sec]');
  var panels = document.querySelectorAll('.panel');
  for (var i = 0; i < links.length; i++) links[i].classList.toggle('active', links[i].getAttribute('data-sec') === sec);
  for (var j = 0; j < panels.length; j++) panels[j].classList.toggle('active', panels[j].getAttribute('data-panel') === sec);
}
document.getElementById('subnav').addEventListener('click', function (e) {
  var a = e.target.closest('a[data-sec]');
  if (!a) return;
  location.hash = a.getAttribute('data-sec');
  showSection(a.getAttribute('data-sec'));
});
if (location.hash && document.querySelector('.panel[data-panel="' + location.hash.slice(1) + '"]')) {
  showSection(location.hash.slice(1));
}
document.getElementById('refresh-sessions').addEventListener('click', loadSessions);
document.getElementById('revoke-all-sessions').addEventListener('click', async function () {
  if (!confirm('Revoke every session for this identity? You will be signed out.')) return;
  try {
    await api('/api/iam/sessions/revoke-all', {
      method: 'POST', headers: {'Content-Type': 'application/json'}, body: '{}'
    });
    location.href = '/logout';
  } catch (error) {
    alert('Session revocation failed: ' + error.message);
  }
});
Promise.resolve(iamReady).then(function () {
  renderIdentityProviders();
  loadSessions();
});

/* --- placement radio --- */
function syncPlacement() {
  var manual = document.getElementById('place_manual').checked;
  document.getElementById('manual-node-field').style.display = manual ? '' : 'none';
  document.getElementById('candidate-field').style.display = manual ? 'none' : '';
}
document.getElementById('place_auto').addEventListener('change', syncPlacement);
document.getElementById('place_manual').addEventListener('change', syncPlacement);

/* --- form state --- */
function val(id) { return document.getElementById(id).value.trim(); }
function setVal(id, v) { document.getElementById(id).value = (v === undefined || v === null) ? '' : v; }
function checked(id) { return document.getElementById(id).checked; }
function csv(id) {
  return val(id) ? val(id).split(',').map(function (s) { return s.trim(); }).filter(Boolean) : [];
}
function collect() {
  var node = document.getElementById('place_manual').checked ? (val('pve_node_manual') || 'pve') : 'auto';
  return {
    provider: val('provider'),
    instance_ttl_minutes: parseInt(val('ttl') || '0', 10) || 0,
    skip_verify_install: false,
    remote_builders: csv('remote_builders'),
    gcp_project: val('gcp_project'),
    gcp_region: val('gcp_region'),
    gcp_zone: val('gcp_zone'),
    gcp_key_file: val('gcp_key_file'),
    aws_region: val('aws_region'),
    aws_zone: val('aws_zone'),
    aws_access_key: val('aws_access_key'),
    aws_secret_key: val('aws_secret_key'),
    pve_endpoint: val('pve_endpoint'),
    pve_node: node,
    pve_nodes: csv('pve_nodes'),
    pve_token_id: val('pve_token_id'),
    pve_token_secret: val('pve_token_secret'),
    pve_username: val('pve_username'),
    pve_password: val('pve_password'),
    pve_insecure: checked('pve_insecure'),
    pve_storage: val('pve_storage'),
    pve_network: val('pve_network'),
    pve_template: val('pve_template'),
    pve_cicustom: val('pve_cicustom'),
    pve_nameserver: val('pve_nameserver'),
    gentoo_mirror: val('gentoo_mirror'),
    portage_sync_uri: val('portage_sync_uri'),
    portage_sync_method: val('portage_sync_method') || 'webrsync',
    make_conf_extra: document.getElementById('make_conf_extra').value,
    build_features: val('build_features'),
    build_mode: 'native-gentoo',
    ssh_key_path: val('ssh_key_path'),
    ssh_user: val('ssh_user'),
    ssh_known_hosts: val('ssh_known_hosts'),
    ssh_insecure_host_key: checked('ssh_insecure'),
    upload_url: val('upload_url'),
    upload_user: val('upload_user'),
    upload_password: val('upload_password'),
    upload_dir: val('upload_dir'),
    server_callback_url: val('callback'),
    builder_binary_path: val('bin_path'),
    builder_binary_url: val('bin_url'),
    builder_binary_sha256: val('bin_sha256')
  };
}
function fill(s) {
  lastSettings = s;
  var externalSecrets = !!s.secret_values_managed_externally;
  function secretText(present) {
    if (externalSecrets) {
      return present
        ? t('set.secret.external.set', 'Managed by deployment environment (configured)')
        : t('set.secret.external.unset', 'Managed by deployment environment (not configured)');
    }
    return present ? t('set.secret.saved', 'Saved; leave empty to keep') : t('set.secret.unset', 'Not set yet');
  }
  ['aws_secret_key', 'pve_password', 'pve_token_secret', 'upload_password'].forEach(function (id) {
    var input = document.getElementById(id);
    if (input) {
      input.disabled = externalSecrets;
      input.placeholder = externalSecrets ? t('set.secret.external.set', 'Managed by deployment environment') : '';
    }
  });
  setVal('provider', s.provider || 'pve');
  setVal('ttl', s.instance_ttl_minutes || 0);
  document.getElementById('verify_install').checked = true;
  setVal('remote_builders', (s.remote_builders || []).join(','));
  setVal('gcp_project', s.gcp_project);
  setVal('gcp_region', s.gcp_region);
  setVal('gcp_zone', s.gcp_zone);
  setVal('gcp_key_file', s.gcp_key_file);
  setVal('aws_region', s.aws_region);
  setVal('aws_zone', s.aws_zone);
  setVal('aws_access_key', s.aws_access_key);
  var awsHint = document.getElementById('aws-secret-hint');
  awsHint.textContent = secretText(s.has_aws_secret_key);
  if (!externalSecrets) document.getElementById('aws_secret_key').placeholder = s.has_aws_secret_key ? t('set.secret.ph', 'Saved — leave empty to keep') : '';
  setVal('pve_endpoint', s.pve_endpoint);
  var auto = !s.pve_node || s.pve_node.toLowerCase() === 'auto';
  document.getElementById('place_auto').checked = auto;
  document.getElementById('place_manual').checked = !auto;
  setVal('pve_node_manual', auto ? '' : s.pve_node);
  syncPlacement();
  setVal('pve_nodes', (s.pve_nodes || []).join(','));
  setVal('pve_token_id', s.pve_token_id);
  setVal('pve_username', s.pve_username);
  var passHint = document.getElementById('pve-pass-hint');
  passHint.textContent = secretText(s.has_pve_password);
  if (!externalSecrets) document.getElementById('pve_password').placeholder = s.has_pve_password ? t('set.secret.ph', 'Saved — leave empty to keep') : '';
  document.getElementById('pve_insecure').checked = !!s.pve_insecure;
  setVal('pve_storage', s.pve_storage);
  setVal('pve_network', s.pve_network);
  setVal('pve_template', s.pve_template);
  setVal('pve_cicustom', s.pve_cicustom);
  setVal('pve_nameserver', s.pve_nameserver);
  setVal('gentoo_mirror', s.gentoo_mirror);
  setVal('portage_sync_uri', s.portage_sync_uri);
  setVal('portage_sync_method', s.portage_sync_method || 'webrsync');
  setVal('make_conf_extra', s.make_conf_extra);
  setVal('build_features', s.build_features);
  setVal('ssh_key_path', s.ssh_key_path);
  setVal('ssh_user', s.ssh_user);
  setVal('ssh_known_hosts', s.ssh_known_hosts);
  document.getElementById('ssh_insecure').checked = !!s.ssh_insecure_host_key;
  setVal('upload_url', s.upload_url);
  setVal('upload_user', s.upload_user);
  setVal('upload_dir', s.upload_dir);
  var upHint = document.getElementById('upload-pass-hint');
  if (upHint) {
    upHint.textContent = secretText(s.has_upload_password);
    if (!externalSecrets) document.getElementById('upload_password').placeholder = s.has_upload_password ? t('set.secret.ph', 'Saved — leave empty to keep') : '';
  }
  setVal('callback', s.server_callback_url);
  setVal('bin_path', s.builder_binary_path);
  setVal('bin_url', s.builder_binary_url);
  setVal('bin_sha256', s.builder_binary_sha256);
  var hint = document.getElementById('secret-hint');
  hint.textContent = secretText(s.has_pve_token_secret);
  if (!externalSecrets) document.getElementById('pve_token_secret').placeholder = s.has_pve_token_secret ? t('set.secret.ph', 'Saved — leave empty to keep') : '';
}
function onLangChange() { if (lastSettings) fill(lastSettings); }
function noteAt(target, text, ok) {
  target.textContent = text;
  target.className = 'save-msg ' + (ok ? 'ok' : 'err');
}
function note(text, ok) { noteAt(msg, text, ok); }

async function saveSettings() {
  var saved = await api('/api/settings/cloud', {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(collect())
  });
  fill(saved);
  setVal('pve_token_secret', '');
  setVal('aws_secret_key', '');
  return saved;
}
async function load() {
  try { fill(await api('/api/settings/cloud')); }
  catch (e) { note(t('set.loadfail', 'Failed to load settings: ') + e.message, false); }
}
form.addEventListener('submit', async function (e) {
  e.preventDefault();
  note(t('set.saving', 'Saving…'), true);
  try {
    await saveSettings();
    setVal('pve_password', '');
    note(t('set.saved', 'Saved — in effect immediately'), true);
  } catch (ex) { note(t('set.savefail', 'Save failed: ') + ex.message, false); }
});

/* --- PVE connection test --- */
document.getElementById('test').addEventListener('click', async function () {
  var tmsg = document.getElementById('test-msg');
  noteAt(tmsg, t('set.testing', 'Connecting to PVE…'), true);
  var card = document.getElementById('test-card');
  var tb = document.getElementById('test-rows');
  try {
    var r = await api('/api/settings/cloud/test', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(collect())
    });
    if (!r.ok) { card.style.display = 'none'; noteAt(tmsg, t('set.testfail', 'Connection failed: ') + (r.error || '-'), false); return; }
    clear(tb);
    (r.nodes || []).forEach(function (n) {
      var tr = el('tr');
      tr.appendChild(el('td', null, n.node));
      var st = el('td'); st.appendChild(statusBadge(n.status)); tr.appendChild(st);
      tr.appendChild(el('td', 'sec', (n.free_mem_gb || 0).toFixed(1) + ' GB'));
      tr.appendChild(el('td', 'sec', ((n.cpu_load || 0) * 100).toFixed(0) + '%'));
      tr.appendChild(el('td', 'sec', n.has_template ? t('set.yes', 'yes') : t('set.no', 'no')));
      tb.appendChild(tr);
    });
    card.style.display = '';
    noteAt(tmsg, t('set.testok', 'Connected — found %d node(s)').replace('%d', (r.nodes || []).length), true);
  } catch (ex) { card.style.display = 'none'; noteAt(tmsg, t('set.testfail', 'Connection failed: ') + ex.message, false); }
});

/* --- full-pipeline test build --- */
var testPoll = null;
document.getElementById('test-build').addEventListener('click', async function () {
  var tmsg = document.getElementById('test-build-msg');
  var box = document.getElementById('test-build-result');
  var pkg = val('test_pkg') || 'app-misc/jq';
  try {
    noteAt(tmsg, t('set.testbuild.saving', 'Saving settings…'), true);
    await saveSettings();
    noteAt(tmsg, t('set.testbuild.submitting', 'Submitting build…'), true);
    var r = await api('/api/builds/submit', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ package_name: pkg, arch: 'amd64' })
    });
    var jobID = r.job_id;
    noteAt(tmsg, t('set.testbuild.submitted', 'Submitted — job ') + jobID.slice(0, 8), true);
    clear(box);
    var line = el('div', 'status gray');
    line.appendChild(el('span', 'dot'));
    var stText = el('span', null, '…');
    line.appendChild(stText);
    var link = el('a', null, ' ' + t('set.testbuild.view', 'View build details'));
    link.href = '/build/' + encodeURIComponent(jobID);
    box.appendChild(line);
    box.appendChild(link);
    if (testPoll) clearInterval(testPoll);
    testPoll = setInterval(async function () {
      try {
        var b = await api('/api/builds/detail?job_id=' + encodeURIComponent(jobID));
        clear(line);
        var badge = statusBadge(b.status);
        line.className = '';
        line.appendChild(badge);
        if (b.error) line.appendChild(el('span', 'sec', ' ' + b.error.slice(0, 160)));
        if (b.status === 'failed' || b.status === 'completed' || b.status === 'success') clearInterval(testPoll);
      } catch (e) { /* keep polling */ }
    }, 5000);
  } catch (ex) { noteAt(tmsg, t('set.testbuild.fail', 'Test build failed: ') + ex.message, false); }
});

/* --- GPG signing --- */
async function loadGPG() {
  try {
    var s = await api('/api/gpg/status');
    document.getElementById('gpg-state').textContent = !s.enabled ? t('set.gpg.off', 'Disabled') : (s.ready ? t('set.gpg.on', 'Ready') : t('set.gpg.wait', 'Waiting for signer'));
    document.getElementById('gpg-keyid').textContent = s.key_id || '-';
    document.getElementById('gpg-mode').textContent = s.mode || '-';
    var q = s.queue || {};
    document.getElementById('gpg-queue').textContent =
      String(q.queued || 0) + ' queued · ' + String(q.claimed || 0) + ' active · ' + String(q.failed || 0) + ' failed';
    if (s.ready) {
      try {
        var r = await fetch('/api/keys/public');
        if (r.ok) document.getElementById('gpg-pubkey').textContent = await r.text();
      } catch (e) {}
    }
  } catch (e) {
    document.getElementById('gpg-state').textContent = '?';
  }
}
loadGPG();

load();
`

// ---------------------------------------------------------------------------
// Image factory
// ---------------------------------------------------------------------------

const imageFactoryContent = `
<div class="page-head">
  <div><h1 data-i18n="factory.h1">Image Factory</h1><p class="sub" data-i18n="factory.sub">Profiles, offline inputs, PVE/PBS provenance, and E2E evidence</p></div>
  <div class="actions"><button class="btn" id="refresh" data-i18n="common.refresh">Refresh</button></div>
</div>
<p class="factory-note" data-i18n="factory.readonly">This surface is read-only. Promotion and rollback remain signed CLI operations.</p>
<div class="stat-grid" id="factory-stats" style="margin-top:16px"></div>
<div id="factory-message"></div>
<h2 class="section-title" data-i18n="factory.catalog">Active Catalog</h2>
<div class="card">
  <div class="table-scroll"><table class="list" aria-label="Image factory profiles">
    <thead><tr><th>Profile</th><th data-i18n="th.arch">Arch</th><th data-i18n="factory.image">Image</th><th>Egress policy</th><th data-i18n="factory.displayModel">Display</th><th data-i18n="factory.sets">Package sets</th><th data-i18n="factory.channel">Channel</th></tr></thead>
    <tbody id="factory-profiles"></tbody>
  </table></div>
  <div id="factory-profile-empty"></div>
</div>
<div class="factory-grid">
  <section>
    <h2 class="section-title" data-i18n="factory.milestones">Milestones</h2>
    <div class="card"><ol class="milestone-list" id="factory-milestones"></ol><div id="factory-milestone-empty"></div></div>
  </section>
  <aside>
    <h2 class="section-title" data-i18n="factory.desktop">Desktop E2E</h2>
    <div class="card"><div class="card-pad" id="factory-desktop"></div></div>
    <h2 class="section-title" data-i18n="factory.blockers">Blockers</h2>
    <div class="card"><div class="card-pad" id="factory-blockers"></div></div>
  </aside>
</div>`

const imageFactoryJS = `
function factoryStat(labelKey, labelEN, value) {
  var tile = el('div', 'stat-tile');
  tile.appendChild(el('h4', null, t(labelKey, labelEN)));
  tile.appendChild(el('div', 'num', String(value)));
  return tile;
}
function labeledLine(key, fallback, value) {
  var p = el('p', 'factory-note');
  p.appendChild(el('strong', null, t(key, fallback) + ': '));
  p.appendChild(document.createTextNode(value || '-'));
  return p;
}
function factoryEvidenceText(item) {
  var detail = item.label || t('factory.evidence', 'Evidence');
  if (item.digest) detail += ' · ' + item.digest;
  if (item.path) detail += ' · ' + item.path;
  if (item.size_bytes) detail += ' · ' + fmtBytes(item.size_bytes);
  if (item.recorded_at) detail += ' · ' + fmtTime(item.recorded_at);
  return detail;
}
function factoryStepMeta(step) {
  var parts = [];
  if (step.started_at) parts.push(t('factory.started', 'Started') + ' ' + fmtTime(step.started_at));
  if (step.completed_at) parts.push(t('factory.finished', 'Finished') + ' ' + fmtTime(step.completed_at));
  if (step.started_at && step.completed_at) parts.push(t('factory.duration', 'Duration') + ' ' + fmtTimeRange(step.started_at, step.completed_at));
  return parts.join(' · ');
}
function renderFactory(data) {
  var catalog = data.catalog || {};
  var profiles = catalog.profiles || [];
  var images = catalog.images || [];
  var bundles = catalog.mirror_bundles || [];
  var stats = document.getElementById('factory-stats'); clear(stats);
  stats.appendChild(factoryStat('factory.profiles', 'Profiles', profiles.length));
  stats.appendChild(factoryStat('factory.images', 'Images', images.length));
  stats.appendChild(factoryStat('factory.bundles', 'Offline bundles', bundles.length));
  stats.appendChild(factoryStat('factory.catalog', 'Catalog', 'v' + (catalog.version || 0)));

  var message = document.getElementById('factory-message'); clear(message);
  if (!data.configured) message.appendChild(el('div', 'card card-pad factory-note', t('factory.notconfigured', 'IMAGE_FACTORY_STATUS_PATH is not configured; catalog data is available, but milestone evidence is not guessed by the UI.')));

  var tbody = document.getElementById('factory-profiles'); clear(tbody);
  var profileEmpty = document.getElementById('factory-profile-empty'); clear(profileEmpty);
  if (!profiles.length) profileEmpty.appendChild(el('div', 'empty', t('factory.none', 'None')));
  profiles.forEach(function (profile) {
    var image = images.find(function (item) { return item.id === profile.image_id; }) || {};
    var tr = el('tr');
    var id = el('td', 'mono', profile.id || '-');
    if (profile.default) id.appendChild(el('span', 'status green', ' · ' + t('factory.default', 'default')));
    tr.appendChild(id);
    tr.appendChild(el('td', 'sec', profile.arch || '-'));
    tr.appendChild(el('td', 'mono sec', profile.image_id || '-'));
    tr.appendChild(el('td', 'mono sec', profile.egress_policy_id || '-'));
    tr.appendChild(el('td', 'sec', image.display_model || '-'));
    tr.appendChild(el('td', 'sec', (profile.package_sets || []).join(', ') || '-'));
    tr.appendChild(el('td', 'sec', profile.channel || '-'));
    tbody.appendChild(tr);
  });

  var status = data.status || {};
  var milestones = status.milestones || [];
  var milestoneList = document.getElementById('factory-milestones'); clear(milestoneList);
  var milestoneEmpty = document.getElementById('factory-milestone-empty'); clear(milestoneEmpty);
  if (!milestones.length) milestoneEmpty.appendChild(el('div', 'empty', t('factory.none', 'None')));
  milestones.forEach(function (milestone) {
    var li = el('li', 'milestone');
    var state = el('div'); state.appendChild(statusBadge(milestone.state)); li.appendChild(state);
    var body = el('div');
    body.appendChild(el('h3', null, milestone.id + ' · ' + milestone.title));
    if (milestone.summary) body.appendChild(el('p', null, milestone.summary));
    if (milestone.completed_at) body.appendChild(el('div', 'milestone-meta', t('factory.completed', 'Completed') + ' ' + fmtTime(milestone.completed_at)));
    if ((milestone.evidence || []).length) {
      var evidence = el('div', 'evidence-list');
      milestone.evidence.forEach(function (item) {
        evidence.appendChild(el('span', null, factoryEvidenceText(item)));
      });
      body.appendChild(evidence);
    }
    if ((milestone.steps || []).length) {
      var details = el('details', 'factory-step-details');
      details.appendChild(el('summary', null, t('factory.stepLogs', 'Stage logs') + ' (' + milestone.steps.length + ')'));
      var stepList = el('div', 'factory-step-list');
      milestone.steps.forEach(function (step) {
        var stepBox = el('div', 'factory-step');
        var head = el('div', 'factory-step-head');
        head.appendChild(statusBadge(step.state));
        head.appendChild(el('strong', null, step.title));
        head.appendChild(el('span', 'factory-step-id', step.id));
        stepBox.appendChild(head);
        var meta = factoryStepMeta(step);
        if (meta) stepBox.appendChild(el('div', 'milestone-meta', meta));
        if (step.summary) stepBox.appendChild(el('p', null, step.summary));
        if (step.log) stepBox.appendChild(el('div', 'factory-step-log', t('factory.log', 'Log') + ': ' + factoryEvidenceText(step.log)));
        stepList.appendChild(stepBox);
      });
      details.appendChild(stepList);
      body.appendChild(details);
    }
    li.appendChild(body); milestoneList.appendChild(li);
  });

  var desktop = status.desktop_e2e || {};
  var desktopBox = document.getElementById('factory-desktop'); clear(desktopBox);
  desktopBox.appendChild(statusBadge(desktop.state || 'not_started'));
  desktopBox.appendChild(labeledLine('factory.desktop.strategy', 'Strategy', desktop.strategy));
  desktopBox.appendChild(labeledLine('factory.desktop.ai', 'AI boundary', desktop.ai_policy));
  desktopBox.appendChild(labeledLine('factory.desktop.runner', 'Runner', desktop.runner));
  desktopBox.appendChild(labeledLine('factory.desktop.display', 'Display', desktop.display));

  var blockers = status.blockers || [];
  var blockerBox = document.getElementById('factory-blockers'); clear(blockerBox);
  if (!blockers.length) blockerBox.appendChild(el('p', 'factory-note', t('factory.noBlockers', 'No blockers are reported by the current status snapshot.')));
  blockers.forEach(function (blocker) {
    var item = el('div', 'blocker');
    item.appendChild(el('strong', null, blocker.code || 'BLOCKED'));
    item.appendChild(el('p', null, blocker.summary || '-'));
    if (blocker.action) item.appendChild(el('p', null, t('factory.action', 'Next') + ': ' + blocker.action));
    blockerBox.appendChild(item);
  });
  if (status.updated_at) document.querySelector('.page-head .sub').textContent = t('factory.updated', 'Evidence updated ') + fmtTime(status.updated_at);
}
async function loadFactory() {
  try { renderFactory(await api('/api/image-factory/status')); }
  catch (error) { showError('factory-message', error); }
}
function onLangChange() { loadFactory(); }
document.getElementById('refresh').addEventListener('click', loadFactory);
loadFactory();
`

// ---------------------------------------------------------------------------
// Public packages, docs, and service status
// ---------------------------------------------------------------------------

const packagesContent = `
<div class="public-head">
  <h1 data-i18n="packages.h1">Packages</h1>
  <p data-i18n="packages.sub">Search binary packages published to the public binhost.</p>
</div>
<div class="card">
  <div class="card-pad">
    <form class="public-search" id="package-search">
      <div class="field">
        <label for="package-query" data-i18n="packages.search">Search</label>
        <input id="package-query" type="text" autocomplete="off" spellcheck="false" placeholder="Package, version, or profile">
      </div>
      <div class="field">
        <label for="package-profile" data-i18n="packages.profile">Profile</label>
        <select id="package-profile"><option value="" data-i18n="packages.all">All profiles</option></select>
      </div>
      <button class="btn blue" type="submit" data-i18n="packages.search">Search</button>
    </form>
  </div>
</div>
<div class="card">
  <div class="package-summary" id="package-summary" aria-live="polite"></div>
  <div class="table-scroll"><table class="list" aria-label="Published packages">
    <thead><tr>
      <th data-i18n="th.package">Package</th><th data-i18n="th.version">Version</th>
      <th data-i18n="packages.profile">Profile</th><th data-i18n="th.arch">Arch</th>
      <th data-i18n="packages.download">Download</th>
    </tr></thead>
    <tbody id="package-rows"></tbody>
  </table></div>
  <div id="package-empty"></div>
  <div class="public-pagination">
    <button class="btn" id="package-prev" type="button" data-i18n="packages.prev">Previous</button>
    <span id="package-page"></span>
    <button class="btn" id="package-next" type="button" data-i18n="packages.next">Next</button>
  </div>
</div>`

const packagesJS = `
var packageLimit = 50;
var packageOffset = 0;
var packageTotal = 0;
var packageParams = new URLSearchParams(location.search);
var packageProfiles = [];
document.getElementById('package-query').value = packageParams.get('q') || '';
document.getElementById('package-query').placeholder = t('packages.search.ph', 'Package, version, or profile');

async function loadPackageProfiles() {
  var response = await publicJSON('/api/public/binhosts');
  packageProfiles = response.binhosts || [];
  renderPackageProfiles();
}
function renderPackageProfiles() {
  var select = document.getElementById('package-profile');
  var selected = select.value || packageParams.get('profile_id') || '';
  clear(select);
  var all = document.createElement('option');
  all.value = '';
  all.textContent = t('packages.all', 'All profiles');
  select.appendChild(all);
  packageProfiles.forEach(function (profile) {
    var option = document.createElement('option');
    option.value = profile.profile_id;
    option.textContent = profile.profile_id +
      (profile.default ? ' · ' + t('packages.default', 'default') : '');
    option.title = profile.binhost_path;
    select.appendChild(option);
  });
  select.value = selected;
}

function renderPackages(response) {
  packageTotal = Number(response.total || 0);
  packageOffset = Number(response.offset || 0);
  var rows = document.getElementById('package-rows');
  var empty = document.getElementById('package-empty');
  clear(rows); clear(empty);
  (response.packages || []).forEach(function (pkg) {
    var row = document.createElement('tr');
    var name = el('td');
    name.appendChild(el('span', 'package-name', pkg.name));
    if (pkg.use_flags && pkg.use_flags.length) {
      var flags = pkg.use_flags.join(' ');
      var flagNode = el('span', 'package-flags', flags.length > 72 ? flags.slice(0, 69) + '…' : flags);
      flagNode.title = flags;
      name.appendChild(flagNode);
    }
    row.appendChild(name);
    row.appendChild(el('td', 'mono', pkg.version));
    var profile = el('td', 'package-profile', pkg.profile_id);
    profile.title = pkg.binhost_path || '';
    row.appendChild(profile);
    row.appendChild(el('td', null, pkg.arch));
    var download = el('td');
    var link = el('a', null, t('packages.download', 'Download'));
    link.href = pkg.download_path;
    download.appendChild(link);
    row.appendChild(download);
    rows.appendChild(row);
  });
  if (!response.packages || !response.packages.length) {
    empty.appendChild(el('div', 'empty', t('packages.none', 'No matching packages were found.')));
  }
  document.getElementById('package-summary').textContent =
    fmtPublicText('packages.count', '%d published packages', packageTotal);
  var start = packageTotal ? packageOffset + 1 : 0;
  var end = Math.min(packageOffset + packageLimit, packageTotal);
  document.getElementById('package-page').textContent =
    fmtPublicText('packages.page', '%d–%d', start, end);
  document.getElementById('package-prev').disabled = packageOffset === 0;
  document.getElementById('package-next').disabled = packageOffset + packageLimit >= packageTotal;
}

async function loadPackages() {
  var params = new URLSearchParams();
  var query = document.getElementById('package-query').value.trim();
  var profile = document.getElementById('package-profile').value;
  if (query) params.set('q', query);
  if (profile) params.set('profile_id', profile);
  params.set('limit', String(packageLimit));
  params.set('offset', String(packageOffset));
  try {
    renderPackages(await publicJSON('/api/public/packages?' + params.toString()));
  } catch (error) {
    clear(document.getElementById('package-rows'));
    var empty = document.getElementById('package-empty'); clear(empty);
    empty.appendChild(el('div', 'empty', t('packages.loadfail', 'Package search failed: ') + ' ' + error.message));
  }
}

function persistPackageSearch() {
  var params = new URLSearchParams();
  var query = document.getElementById('package-query').value.trim();
  var profile = document.getElementById('package-profile').value;
  if (query) params.set('q', query);
  if (profile) params.set('profile_id', profile);
  history.replaceState(null, '', '/packages' + (params.toString() ? '?' + params.toString() : ''));
}
document.getElementById('package-search').addEventListener('submit', function (event) {
  event.preventDefault(); packageOffset = 0; persistPackageSearch(); loadPackages();
});
document.getElementById('package-prev').addEventListener('click', function () {
  packageOffset = Math.max(0, packageOffset - packageLimit); loadPackages();
});
document.getElementById('package-next').addEventListener('click', function () {
  if (packageOffset + packageLimit < packageTotal) packageOffset += packageLimit;
  loadPackages();
});
function onLangChange() {
  document.getElementById('package-query').placeholder = t('packages.search.ph', 'Package, version, or profile');
  renderPackageProfiles();
  loadPackages();
}
loadPackageProfiles().catch(function () {}).then(loadPackages);
`

const docsContent = `
<div class="public-head">
  <h1 data-i18n="docs.h1">Documentation</h1>
  <p data-i18n="docs.sub">Choose a binhost, establish signing trust, and configure Portage without signing in.</p>
</div>
<div class="card public-docs"><div class="card-pad docs-body">
  <h2 data-i18n="docs.consume">Consume binary packages</h2>
  <p data-i18n="docs.consume.p">First use the Packages page to identify the binhost matching this machine's ABI and profile. Each profile is an independent PKGDIR; /binpkgs is not an aggregate repository.</p>
  <p><a href="/packages" data-i18n="docs.browse">Browse published packages and profiles</a></p>
  <h2 data-i18n="docs.client">Use the Portage Engine client</h2>
  <p data-i18n="docs.client.p">The client resolves the profile's exact official-style path from the public inventory and writes binrepos.conf. Omitting profile-id selects the default profile:</p>
  <pre>sudo portage-client configure \
  -server=https://SERVER \
  -profile-id=pe/amd64/glibc/systemd/base-v1

emerge --getbinpkg app-misc/jq</pre>
  <h2 data-i18n="docs.manual">Configure Portage manually</h2>
  <p data-i18n="docs.manual.p">You can instead create binrepos.conf directly. Replace the example with the Binhost path shown on the Packages page:</p>
  <pre>[portage-engine]
priority = 1
sync-uri = https://SERVER/binpkgs/releases/amd64/binpackages/23.0/TARGET
verify-signature = true</pre>
  <h2 data-i18n="docs.gpg">Establish signing trust</h2>
  <p data-i18n="docs.gpg.p">Configure writes binrepos.conf only; it does not import or trust a key. Obtain the release public key and full fingerprint through an independent operator-controlled channel, import it into /etc/portage/gnupg, and establish trust. Never obtain both packages and the trust root over the same unauthenticated HTTP connection.</p>
  <p class="notice" data-i18n="docs.verify.note">“Published” in the web catalog does not replace client-side signature verification.</p>
  <h2 data-i18n="docs.build">Request a build</h2>
  <p data-i18n="docs.build.p">Browsing and installing published packages needs no account. Submitting a build requires sign-in and access to a project:</p>
  <pre>portage-client build \
  -server=https://SERVER \
  -project=PROJECT \
  -profile-id=pe/amd64/glibc/systemd/base-v1 \
  -package=app-misc/jq \
  -wait</pre>
  <p data-i18n="docs.build.p2">A package is published only after isolated install and signature verification; publication atomically refreshes that profile's Packages index.</p>
</div></div>`

const docsJS = `/* static public page */`

const statusContent = `
<div class="public-head">
  <h1 data-i18n="status.h1">Service status</h1>
  <p data-i18n="status.sub">Public, redacted availability for Portage Engine services.</p>
</div>
<div class="card">
  <div class="status-overall">
    <div><strong id="status-title">Loading status…</strong><p id="status-updated"></p></div>
    <span data-i18n="status.refresh">Refreshes automatically every 30 seconds</span>
  </div>
  <div class="status-list" id="status-components"></div>
</div>`

const statusJS = `
function statusLabel(state) {
  if (state === 'operational') return t('status.operational', 'All systems operational');
  if (state === 'degraded') return t('status.degraded', 'Some services are degraded');
  return t('status.unavailable', 'Status service unavailable');
}
function statusIndicator(state) {
  var color = state === 'operational' ? 'green' : (state === 'degraded' ? 'orange' : 'red');
  var wrap = el('span', 'status ' + color);
  wrap.appendChild(el('span', 'dot'));
  wrap.appendChild(el('span', null, state === 'operational' ?
    t('status.state.operational', 'Operational') : t('status.state.degraded', 'Degraded')));
  return wrap;
}
function componentLabel(name) {
  if (name === 'Web and API') return t('status.component.api', name);
  if (name === 'Package repository') return t('status.component.repository', name);
  if (name === 'Build service') return t('status.component.build', name);
  return name;
}
async function loadPublicStatus() {
  var title = document.getElementById('status-title');
  var updated = document.getElementById('status-updated');
  var components = document.getElementById('status-components');
  try {
    var response = await publicJSON('/api/public/status');
    title.textContent = statusLabel(response.status);
    updated.textContent = t('status.updated', 'Updated ') + fmtPublicTime(response.updated_at) +
      (response.version ? ' · ' + t('status.version', 'Version ') + response.version : '');
    clear(components);
    (response.components || []).forEach(function (component) {
      var row = el('div', 'status-row');
      row.appendChild(el('span', null, componentLabel(component.name)));
      row.appendChild(statusIndicator(component.status));
      components.appendChild(row);
    });
  } catch (error) {
    title.textContent = statusLabel('unavailable');
    updated.textContent = error.message;
    clear(components);
  }
}
function onLangChange() { loadPublicStatus(); }
loadPublicStatus();
setInterval(loadPublicStatus, 30000);
`

// shellHTML is the full-screen web terminal page.
const shellHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title data-i18n="title.shell">Shell — Portage Engine</title>
<link rel="stylesheet" href="/static/apple.css?v=2">
<link rel="stylesheet" href="/static/xterm.css">
<style>
  .shell-head { display: flex; align-items: center; gap: 12px; padding: 10px 16px; border-bottom: .5px solid var(--labelDivider); }
  .shell-head .brand { font: var(--body-emphasized); }
  .shell-head .iid { font: 500 12px var(--font-mono); color: var(--systemSecondary); }
  #term { padding: 12px; height: calc(100vh - 46px); box-sizing: border-box; background: #000; }
  .xterm { height: 100%; }
</style>
</head>
<body>
<div class="shell-head">
  <a class="btn" href="/monitor" data-i18n="shell.back">Back</a>
  <span class="brand" data-i18n="shell.title">Instance Shell</span>
  <span class="iid" id="iid" data-instance-id="{{.InstanceID}}"></span>
  <span class="save-msg" id="shell-status"></span>
</div>
<div id="term"></div>
<script src="/static/xterm.js"></script>
<script>` + i18nJS + `
var instanceID = document.getElementById('iid').getAttribute('data-instance-id');
document.getElementById('iid').textContent = instanceID;
var statusEl = document.getElementById('shell-status');
var term = new Terminal({ fontSize: 13, fontFamily: 'ui-monospace, SF Mono, Menlo, monospace', cursorBlink: true, cols: 220, rows: 50, theme: { background: '#000000' } });
term.open(document.getElementById('term'));
var proto = location.protocol === 'https:' ? 'wss://' : 'ws://';
var ws = new WebSocket(proto + location.host + '/api/shell?id=' + encodeURIComponent(instanceID));
ws.binaryType = 'arraybuffer';
ws.onopen = function () { statusEl.textContent = t('shell.connected', 'connected'); statusEl.className = 'save-msg ok'; term.focus(); };
ws.onclose = function () { statusEl.textContent = t('shell.closed', 'disconnected'); statusEl.className = 'save-msg err'; };
ws.onerror = function () { statusEl.textContent = t('shell.error', 'connection error'); statusEl.className = 'save-msg err'; };
ws.onmessage = function (ev) {
  if (typeof ev.data === 'string') term.write(ev.data);
  else term.write(new Uint8Array(ev.data));
};
term.onData(function (data) { if (ws.readyState === 1) ws.send(data); });
</script>
</body>
</html>`

// Assembled console and public pages.
var (
	overviewHTML     = appPage("Overview", "title.overview", "overview", overviewContent, overviewJS)
	buildsPageHTML   = appPage("Builds", "title.builds", "builds", buildsContent, buildsJS)
	buildDetailHTML  = appPage("Build Details", "title.detail", "builds", buildDetailContent, buildDetailJS)
	logsPageHTML     = appPage("Build Logs", "title.logs", "builds", logsContent, logsJS)
	monitorHTML      = appPage("Build Nodes", "title.monitor", "monitor", monitorContent, monitorJS)
	imageFactoryHTML = appPage("Image Factory", "title.factory", "image-factory", imageFactoryContent, imageFactoryJS)
	settingsHTML     = appPage("Settings", "title.settings", "settings", settingsContent, settingsJS)
	packagesHTML     = publicPage("Packages", "title.packages", "packages", packagesContent, packagesJS)
	docsHTML         = publicPage("Documentation", "title.docs", "docs", docsContent, docsJS)
	statusHTML       = publicPage("Service Status", "title.status", "status", statusContent, statusJS)
)
