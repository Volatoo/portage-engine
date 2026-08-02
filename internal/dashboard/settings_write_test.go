package dashboard

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// /settings is the only surface in the console that overwrites credentials
// every build depends on, and both of its failure modes are quiet: a second
// activation during a write used to send a second write, and a rejection the
// prose could not pin to a control used to change nothing the reader could
// see. Neither is visible in the source at a glance, so the gates below run
// the page's own script — i18nJS + baseJS + settingsJS, the exact string
// appPage() ships — against a document stub small enough to read, and drive it
// through the four activations the interaction contract names.
//
// Red by: dropping the savePending guard from saveSettings(), moving the
// clearSubmittedSecrets() call above the finally that releases the flag, or
// letting reportSaveFailure() return without moving focus when no control
// claimed the message.

// settingsDOMStub is a document, not a DOM: elements are auto-vivified by id,
// selectors answer empty, and ancestry is whatever a scenario declares. That
// is exactly the surface the settings script touches, and keeping it this
// small is what makes the scenarios below readable as descriptions of an
// interaction rather than as browser plumbing.
const settingsDOMStub = `
var PE = { requests: [], secretClears: [], lastFocus: null };

function Elem(id, tag) {
  this.id = id || '';
  this.tagName = (tag || 'div').toUpperCase();
  this._value = '';
  this.checked = false;
  this.disabled = false;
  this.hidden = false;
  this.textContent = '';
  this.className = '';
  this.placeholder = '';
  this.href = '';
  this.style = {};
  this.attributes = {};
  this.children = [];
  this.listeners = {};
  this._ancestors = {};
  this.focused = 0;
  this.firstChild = null;
  this.parentNode = null;
}
// A real control coerces whatever is assigned to it; the settings form assigns
// numbers and then trims them.
Object.defineProperty(Elem.prototype, 'value', {
  get: function () { return this._value; },
  set: function (v) { this._value = (v === null || v === undefined) ? '' : String(v); }
});
Elem.prototype.setAttribute = function (k, v) { this.attributes[k] = String(v); };
Elem.prototype.getAttribute = function (k) {
  return Object.prototype.hasOwnProperty.call(this.attributes, k) ? this.attributes[k] : null;
};
Elem.prototype.removeAttribute = function (k) { delete this.attributes[k]; };
Elem.prototype.addEventListener = function (type, fn) {
  (this.listeners[type] = this.listeners[type] || []).push(fn);
};
Elem.prototype.removeEventListener = function () {};
Elem.prototype.appendChild = function (child) {
  this.children.push(child);
  this.firstChild = this.children[0];
  child.parentNode = this;
  return child;
};
Elem.prototype.removeChild = function (child) {
  this.children = this.children.filter(function (c) { return c !== child; });
  this.firstChild = this.children[0] || null;
  return child;
};
Elem.prototype.remove = function () {};
Elem.prototype.focus = function () { this.focused++; PE.lastFocus = this; };
Elem.prototype.closest = function (selector) { return this._ancestors[selector] || null; };
Elem.prototype.querySelector = function () { return null; };
Elem.prototype.querySelectorAll = function () { return []; };
Elem.prototype.dispatch = function (type, event) {
  var fns = this.listeners[type] || [];
  for (var i = 0; i < fns.length; i++) fns[i].call(this, event);
};

var byID = {};
function peGet(id) {
  if (!byID[id]) {
    byID[id] = new Elem(id);
    byID[id].parentNode = new Elem('', 'div');
  }
  return byID[id];
}

globalThis.document = {
  hidden: false,
  documentElement: new Elem('html', 'html'),
  body: new Elem('body', 'body'),
  getElementById: peGet,
  createElement: function (tag) { return new Elem('', tag); },
  createTextNode: function (text) { var n = new Elem('', 'text'); n.textContent = text; return n; },
  querySelector: function () { return null; },
  querySelectorAll: function () { return []; },
  addEventListener: function () {}
};
globalThis.window = { addEventListener: function () {}, peAuthentication: 'local' };
globalThis.location = { hash: '', href: '/settings', pathname: '/settings' };
globalThis.localStorage = { getItem: function () { return null; }, setItem: function () {} };
globalThis.confirm = function () { return false; };
globalThis.alert = function () {};
globalThis.Headers = function (init) {
  this._h = Object.assign({}, init || {});
  this.set = function (k, v) { this._h[k] = v; };
  this.get = function (k) { return this._h[k]; };
};

// Every request is recorded. Reads settle at once; the settings write is held
// open so a scenario decides what "in flight" means, which is the only way to
// observe what a second activation does while the first has not answered.
globalThis.fetch = function (path, opts) {
  var record = { path: path, method: (opts && opts.method) || 'GET', body: opts && opts.body };
  PE.requests.push(record);
  return new Promise(function (resolve) {
    record.settle = function (payload, status) {
      resolve({
        ok: status === undefined || status < 400,
        status: status === undefined ? 200 : status,
        json: function () { return Promise.resolve(payload || {}); },
        text: function () {
          return Promise.resolve(typeof payload === 'string' ? payload : JSON.stringify(payload || {}));
        }
      });
    };
    if (path.indexOf('/api/settings/cloud') !== 0 || record.method !== 'PUT') record.settle({});
  });
};

globalThis.peGet = peGet;
globalThis.PE = PE;
globalThis.writes = function () {
  return PE.requests.filter(function (r) { return r.path === '/api/settings/cloud' && r.method === 'PUT'; });
};
// Drains the microtask queue and one macrotask turn: everything the page does
// between activations is promise work, so this is "the page has caught up".
globalThis.settled = async function () {
  for (var i = 0; i < 50; i++) await Promise.resolve();
  await new Promise(function (r) { setTimeout(r, 0); });
  for (var j = 0; j < 50; j++) await Promise.resolve();
};
globalThis.submitEvent = function () {
  return { prevented: false, preventDefault: function () { this.prevented = true; } };
};
globalThis.report = function (o) {
  console.log('PE_RESULT ' + JSON.stringify(o));
  process.exit(0);
};
process.on('unhandledRejection', function (e) {
  console.log('PE_RESULT ' + JSON.stringify({ unhandled: String(e && e.stack || e) }));
  process.exit(0);
});
`

// runSettingsScenario concatenates the stub, the page's real script and the
// scenario into one file and reads back the single reported object.
func runSettingsScenario(t *testing.T, scenario string) map[string]any {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not on PATH; the settings write contract cannot be observed here")
	}
	path := filepath.Join(t.TempDir(), "settings-scenario.js")
	source := settingsDOMStub + i18nJS + baseJS + settingsJS + scenario
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatalf("writing the scenario: %v", err)
	}
	out, err := exec.Command(node, path).CombinedOutput()
	if err != nil {
		t.Fatalf("running the scenario: %v\n%s", err, out)
	}
	var line string
	for _, candidate := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(candidate, "PE_RESULT ") {
			line = strings.TrimPrefix(candidate, "PE_RESULT ")
		}
	}
	if line == "" {
		t.Fatalf("the scenario reported nothing:\n%s", out)
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(line), &result); err != nil {
		t.Fatalf("unreadable report %q: %v", line, err)
	}
	if bad, ok := result["unhandled"]; ok {
		t.Fatalf("the page threw: %v", bad)
	}
	return result
}

func number(t *testing.T, result map[string]any, key string) float64 {
	t.Helper()
	v, ok := result[key].(float64)
	if !ok {
		t.Fatalf("the scenario did not report %q (got %#v)", key, result[key])
	}
	return v
}

// A double click, a held Enter, an Enter that only confirmed an IME candidate
// and the form's second writer are four ways to ask for the same write. On a
// slow network they used to be four writes, and the later ones carried the
// secret inputs an earlier one had already emptied.
func TestSettingsWriteDiscardsEveryActivationDuringFlight(t *testing.T) {
	result := runSettingsScenario(t, `
(async function () {
  var form = peGet('settings-form');
  var save = peGet('save');
  await settled();

  peGet('pve_token_secret').value = 'typed-token';
  peGet('aws_secret_key').value = 'typed-aws';
  peGet('pve_password').value = 'typed-password';

  form.dispatch('submit', submitEvent());
  await settled();
  var afterFirst = writes().length;
  var inFlight = {
    ariaDisabled: save.getAttribute('aria-disabled'),
    ariaBusy: save.getAttribute('aria-busy'),
    liveRegion: peGet('msg').textContent,
    buttonText: save.textContent
  };

  // The second click of a double click, ~80ms into a 3s write.
  form.dispatch('submit', submitEvent());
  await settled();
  var afterDoubleClick = writes().length;

  // A held Enter repeats the implicit submission.
  var heldEnter = { key: 'Enter', isComposing: false, preventDefault: function () { this.prevented = true; } };
  form.dispatch('keydown', heldEnter);
  form.dispatch('submit', submitEvent());
  form.dispatch('keydown', heldEnter);
  form.dispatch('submit', submitEvent());
  await settled();
  var afterHeldEnter = writes().length;

  // The Enter that only confirmed an IME candidate never becomes a submission.
  var imeEnter = { key: 'Enter', isComposing: true, prevented: false, preventDefault: function () { this.prevented = true; } };
  form.dispatch('keydown', imeEnter);
  if (!imeEnter.prevented) form.dispatch('submit', submitEvent());
  await settled();
  var afterIMEEnter = writes().length;

  // Start Test Build writes the same resource from a different control.
  peGet('test-build').dispatch('click', { isComposing: false, preventDefault: function () {} });
  await settled();
  var afterSecondWriter = writes().length;

  var pendingSecrets = {
    token: peGet('pve_token_secret').value,
    aws: peGet('aws_secret_key').value,
    password: peGet('pve_password').value
  };

  writes()[0].settle({});
  await settled();

  report({
    afterFirst: afterFirst,
    afterDoubleClick: afterDoubleClick,
    afterHeldEnter: afterHeldEnter,
    afterIMEEnter: afterIMEEnter,
    afterSecondWriter: afterSecondWriter,
    imeEnterPrevented: imeEnter.prevented,
    inFlight: inFlight,
    pendingSecrets: pendingSecrets,
    secondWriterMessage: peGet('test-build-msg').textContent,
    settledLiveRegion: peGet('msg').textContent,
    idleAriaDisabled: save.getAttribute('aria-disabled'),
    idleAriaBusy: save.getAttribute('aria-busy')
  });
})();
`)

	if got := number(t, result, "afterFirst"); got != 1 {
		t.Fatalf("the first activation sent %v writes, want 1", got)
	}
	for _, activation := range []string{"afterDoubleClick", "afterHeldEnter", "afterIMEEnter", "afterSecondWriter"} {
		if got := number(t, result, activation); got != 1 {
			t.Errorf("%s: %v writes reached the server while the first was in flight, want 1 — "+
				"the extra activation was replayed instead of discarded", activation, got)
		}
	}
	if result["imeEnterPrevented"] != true {
		t.Error("an Enter that only confirmed an IME candidate was allowed to submit the form")
	}

	inFlight, _ := result["inFlight"].(map[string]any)
	if inFlight["ariaDisabled"] != "true" {
		t.Errorf("aria-disabled during the write is %v, want \"true\"", inFlight["ariaDisabled"])
	}
	if inFlight["ariaBusy"] != "true" {
		t.Errorf("aria-busy during the write is %v, want \"true\" — the control never said it was mid-operation",
			inFlight["ariaBusy"])
	}
	if progress, _ := inFlight["liveRegion"].(string); !strings.Contains(progress, "Saving") {
		t.Errorf("the footer live region says %q during the write; progress is announced there, not on the control", progress)
	}
	if text, _ := inFlight["buttonText"].(string); text != "" {
		t.Errorf("the save button narrated %q; a control that carries its own status is re-read on every focus", text)
	}

	// The whole harm of a replayed activation is the payload it collects, so
	// the secrets must still be in the inputs for as long as anything is in
	// flight.
	pending, _ := result["pendingSecrets"].(map[string]any)
	for field, want := range map[string]string{
		"token": "typed-token", "aws": "typed-aws", "password": "typed-password",
	} {
		if pending[field] != want {
			t.Errorf("%s input holds %v while a write is pending, want %q — a write collected now "+
				"would send a blank, and blank is the wire spelling of \"keep the stored one\"",
				field, pending[field], want)
		}
	}
	if msg, _ := result["secondWriterMessage"].(string); !strings.Contains(msg, "already in flight") {
		t.Errorf("Start Test Build stood down silently; it said %q", msg)
	}
	if result["idleAriaDisabled"] != "false" {
		t.Errorf("aria-disabled after the write is %v, want \"false\"", result["idleAriaDisabled"])
	}
	if result["idleAriaBusy"] != nil {
		t.Errorf("aria-busy survived the write as %v; it must be removed once the control is idle", result["idleAriaBusy"])
	}
	if done, _ := result["settledLiveRegion"].(string); !strings.Contains(done, "Saved") {
		t.Errorf("the footer live region says %q after a successful write", done)
	}
}

// Secrets are emptied by a successful write, and blank is what the server
// reads as "keep the stored one". Emptying them before the form is idle hands
// the next collect() a blank for a credential the operator just typed.
func TestSettingsSecretsAreEmptiedOnlyOnceNoWriteIsPending(t *testing.T) {
	result := runSettingsScenario(t, `
(async function () {
  var form = peGet('settings-form');
  await settled();

  // Every emptying of a secret input records whether a write was still in
  // flight at that instant.
  var passThrough = setVal;
  setVal = function (id, v) {
    var secret = id === 'pve_token_secret' || id === 'aws_secret_key' || id === 'pve_password';
    if (secret && (v === '' || v === undefined || v === null)) {
      PE.secretClears.push({ id: id, pending: savePending });
    }
    return passThrough(id, v);
  };

  peGet('pve_token_secret').value = 'typed-token';
  peGet('aws_secret_key').value = 'typed-aws';
  peGet('pve_password').value = 'typed-password';

  form.dispatch('submit', submitEvent());
  await settled();
  var clearsWhilePending = PE.secretClears.length;

  writes()[0].settle({});
  await settled();

  report({
    clearsWhilePending: clearsWhilePending,
    clears: PE.secretClears,
    afterWrite: {
      token: peGet('pve_token_secret').value,
      aws: peGet('aws_secret_key').value,
      password: peGet('pve_password').value
    }
  });
})();
`)

	if got := number(t, result, "clearsWhilePending"); got != 0 {
		t.Errorf("%v secret inputs were emptied before the write answered", got)
	}
	clears, _ := result["clears"].([]any)
	if len(clears) == 0 {
		t.Fatal("a successful write left the typed secrets in the form; they are meant to be emptied once it is idle")
	}
	for _, entry := range clears {
		clear, _ := entry.(map[string]any)
		if clear["pending"] == true {
			t.Errorf("%v was emptied while a write was still in flight", clear["id"])
		}
	}
	after, _ := result["afterWrite"].(map[string]any)
	for _, field := range []string{"token", "aws", "password"} {
		if after[field] != "" {
			t.Errorf("%s still holds %v after a successful write", field, after[field])
		}
	}
}

// The server answers a rejection in prose. When the prose names a field the
// panel holding it opens and focus lands on the control; when it names none —
// or names one nobody can reach — the rejection used to change nothing the
// reader could see, and a save that wipes credentials looked identical to a
// save that did nothing.
func TestSettingsRejectionIsNeverSilent(t *testing.T) {
	result := runSettingsScenario(t, `
async function reject(message) {
  PE.requests.length = 0;
  PE.lastFocus = null;
  peGet('settings-form').dispatch('submit', submitEvent());
  await settled();
  writes()[0].settle(message, 400);
  await settled();
  return {
    message: peGet('msg').textContent,
    focused: PE.lastFocus ? PE.lastFocus.id : null,
    token: peGet('pve_token_secret').value,
    aws: peGet('aws_secret_key').value
  };
}
(async function () {
  await settled();
  peGet('pve_token_secret').value = 'typed-token';
  peGet('aws_secret_key').value = 'typed-aws';

  // A control the prose names, sitting on a panel that is not the open one.
  var endpoint = peGet('pve_endpoint');
  var panel = new Elem('', 'section');
  panel.setAttribute('data-panel', 'pve');
  endpoint._ancestors = { '.field': new Elem('', 'div'), '.panel': panel };
  var attributed = await reject('pve_endpoint must be an absolute https URL');

  // Prose that names nothing this form can point at.
  var unattributed = await reject('the control plane refused this write');

  // Prose that names a control nobody can reach: verification is mandatory,
  // so its checkbox ships disabled.
  peGet('verify_install').disabled = true;
  var unreachable = await reject('skip_verify_install is no longer supported: verification is mandatory');

  report({
    attributed: attributed,
    attributedInvalid: endpoint.getAttribute('aria-invalid'),
    attributedDescribedBy: endpoint.getAttribute('aria-describedby'),
    unattributed: unattributed,
    unreachable: unreachable,
    unreachableInvalid: peGet('verify_install').getAttribute('aria-invalid')
  });
})();
`)

	attributed, _ := result["attributed"].(map[string]any)
	if attributed["focused"] != "pve_endpoint" {
		t.Errorf("a rejection naming pve_endpoint focused %v", attributed["focused"])
	}
	if result["attributedInvalid"] != "true" || result["attributedDescribedBy"] != "pve_endpoint-error" {
		t.Errorf("the named control is not wired to its hint: aria-invalid=%v aria-describedby=%v",
			result["attributedInvalid"], result["attributedDescribedBy"])
	}

	for _, name := range []string{"unattributed", "unreachable"} {
		fallback, _ := result[name].(map[string]any)
		text, _ := fallback["message"].(string)
		if !strings.Contains(text, "Save failed") {
			t.Errorf("%s: the form-level live region says %q; it must say the save failed", name, text)
		}
		if fallback["focused"] != "msg" {
			t.Errorf("%s: focus went to %v instead of the message that says the save failed — "+
				"the form looked unchanged and the rejection was silent", name, fallback["focused"])
		}
		if fallback["token"] != "typed-token" || fallback["aws"] != "typed-aws" {
			t.Errorf("%s: a rejected write emptied the typed secrets (token=%v aws=%v)",
				name, fallback["token"], fallback["aws"])
		}
	}
	if text, _ := result["unattributed"].(map[string]any)["message"].(string); !strings.Contains(text, "control plane refused") {
		t.Errorf("the server's own sentence never reached the reader: %q", text)
	}
	if result["unreachableInvalid"] != nil {
		t.Errorf("aria-invalid was written onto a disabled control (%v); it takes no focus and reads to nobody",
			result["unreachableInvalid"])
	}
}

// Red by: dropping id="pve" from the Proxmox panel, or renaming a subnav
// href without renaming the panel it points at.
func TestSettingsSubnavFragmentsResolveToAPanel(t *testing.T) {
	panelIDs := map[string]bool{}
	for _, panel := range regexp.MustCompile(`<section class="panel" id="([^"]+)" data-panel="([^"]+)"`).
		FindAllStringSubmatch(settingsContent, -1) {
		if panel[1] != panel[2] {
			t.Errorf("panel id=%q carries data-panel=%q; the fragment and the script must name the same section",
				panel[1], panel[2])
		}
		panelIDs[panel[1]] = true
	}
	if len(panelIDs) == 0 {
		t.Fatal("no settings panel declares an id")
	}
	links := regexp.MustCompile(`<a href="#([^"]+)" data-sec="([^"]+)"`).FindAllStringSubmatch(settingsContent, -1)
	if len(links) != len(panelIDs) {
		t.Errorf("%d subnav links for %d panels", len(links), len(panelIDs))
	}
	for _, link := range links {
		if link[1] != link[2] {
			t.Errorf("subnav link href=#%s carries data-sec=%q", link[1], link[2])
		}
		if !panelIDs[link[1]] {
			t.Errorf("subnav href=#%s points at no element; the browser has nothing to move reading position to", link[1])
		}
	}
}

// The credential rejection on /login is about the pair of inputs, so both name
// it as their description. Red by: removing aria-describedby="err" from either
// input, or dropping the aria-invalid the script sets on a rejection.
func TestLoginErrorIsAssociatedWithTheCredentialInputs(t *testing.T) {
	for _, input := range []string{`id="u"`, `id="p"`} {
		field := regexp.MustCompile(`<input[^>]*` + regexp.QuoteMeta(input) + `[^>]*>`).FindString(loginHTML)
		if field == "" {
			t.Fatalf("the login form no longer has an input with %s", input)
		}
		if !strings.Contains(field, `aria-describedby="err"`) {
			t.Errorf("%s is not described by the error it will be rejected with: %s", input, field)
		}
	}
	if !strings.Contains(loginHTML, `id="err"`) {
		t.Fatal("the login error region is gone")
	}
	if !strings.Contains(loginHTML, "markCredentials(true)") {
		t.Error("a rejected sign-in no longer marks the credential inputs aria-invalid")
	}
}
