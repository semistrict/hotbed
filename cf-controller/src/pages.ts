// The server-rendered sign-in pages (/auth/*): sign in, choose an
// organization, enter a sign-up code. Each is one dark card on one shell.
import { escapeHtml } from "./encoding.ts";

const BASE_CSS = `
  body{margin:0;min-height:100vh;display:grid;place-items:center;background:#0b0d12;color:#e5e7eb;font-family:system-ui,-apple-system,sans-serif;padding:24px;box-sizing:border-box}
  .card{width:100%;padding:28px;border:1px solid #1f2430;border-radius:14px;background:#121620;box-sizing:border-box}
  h1{margin:0 0 6px;font-size:19px;font-weight:600}
  .sub{margin:0 0 20px;color:#9ca3af;font-size:13px;line-height:1.45}
  .err{color:#f87171;margin:0 0 14px;font-size:13px;padding:10px 12px;border:1px solid #7f1d1d;background:#1f1115;border-radius:8px}
  input[type=text],input[type=password]{width:100%;padding:10px 12px;border-radius:8px;border:1px solid #2b3242;background:#0b0d12;color:#f9fafb;font-size:14px;box-sizing:border-box;margin-bottom:12px}
  .primary{display:block;width:100%;text-align:center;text-decoration:none;padding:10px 12px;border-radius:8px;border:0;background:#3b82f6;color:#fff;font-weight:600;font-size:14px;cursor:pointer;box-sizing:border-box}
  .primary:hover{background:#2563eb}
  .footer{margin-top:18px;display:flex;justify-content:space-between;align-items:center;font-size:12px;color:#6b7280}
  .logout{background:none;border:0;color:#9ca3af;cursor:pointer;font-size:12px;padding:0;text-decoration:underline}
  .logout:hover{color:#e5e7eb}`;

const SIGN_OUT_FOOTER = `<div class="footer">
    <span>hotbed cockpit</span>
    <form method="POST" action="/auth/logout"><button type="submit" class="logout">Sign out</button></form>
  </div>`;

/** A page of one card, maxWidth wide; card is its whole element (a <div> or <form> with class "card"). */
function cardPage(opts: { title: string; maxWidth: number; css?: string; card: string }): string {
	return `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8" />
<meta name="viewport" content="width=device-width,initial-scale=1" />
<title>${escapeHtml(opts.title)}</title>
<style>${BASE_CSS}
  .card{max-width:${opts.maxWidth}px}${opts.css ?? ""}
</style>
</head>
<body>
${opts.card}
</body>
</html>`;
}

function errorLine(error?: string): string {
	return error ? `<p class="err">${escapeHtml(error)}</p>` : "";
}

function returnToInput(returnTo?: string): string {
	return `<input type="hidden" name="return_to" value="${escapeHtml(returnTo || "/")}" />`;
}

export function renderLoginPage(error?: string, workosEnabled = false, returnTo = "/"): string {
	if (workosEnabled) {
		const googleHref =
			returnTo && returnTo !== "/"
				? `/auth/login?return_to=${encodeURIComponent(returnTo)}`
				: "/auth/login";
		return cardPage({
			title: "hotbed cockpit — sign in",
			maxWidth: 360,
			card: `<div class="card">
  <h1>hotbed cockpit</h1>
  <p class="sub">Sign in with your Google account to continue.</p>
  ${errorLine(error)}
  <a href="${escapeHtml(googleHref)}" class="primary">Sign in with Google</a>
</div>`,
		});
	}
	return cardPage({
		title: "hotbed cockpit — sign in",
		maxWidth: 360,
		card: `<form method="POST" action="/auth/login" class="card">
  <h1>hotbed cockpit</h1>
  <p class="sub">Enter your controller owner token to sign in.</p>
  ${errorLine(error)}
  ${returnToInput(returnTo)}
  <input type="password" name="token" placeholder="Owner token" autocomplete="current-password" autofocus required />
  <button type="submit" class="primary">Sign in</button>
</form>`,
	});
}

export function renderOrgPickerPage(opts: {
	email: string;
	organizations: Array<{ id: string; name: string; role?: string }>;
	pendingSelection?: boolean;
	error?: string;
	returnTo?: string;
}): string {
	const actionPath = opts.pendingSelection ? "/auth/org/select-pending" : "/auth/org/switch";
	const orgButtons = opts.organizations
		.map(
			(o) => `<form method="POST" action="${actionPath}" class="org-row">
  <input type="hidden" name="organization_id" value="${escapeHtml(o.id)}" />
  ${returnToInput(opts.returnTo)}
  <button type="submit" class="org-btn">
    <span class="org-name">${escapeHtml(o.name)}</span>
    <span class="org-meta">${escapeHtml(o.role || "member")} · ${escapeHtml(o.id)}</span>
  </button>
</form>`,
		)
		.join("\n");
	const createSection = opts.pendingSelection
		? ""
		: `<div class="divider"><span>or create a new organization</span></div>
<form method="POST" action="/auth/org/create">
  ${returnToInput(opts.returnTo)}
  <input type="text" name="name" placeholder="Organization name (e.g. Acme)" maxlength="100" required />
  <button type="submit" class="primary">Create organization</button>
</form>`;
	return cardPage({
		title: "hotbed — select organization",
		maxWidth: 440,
		css: `
  .org-row{margin-bottom:8px}
  .org-btn{width:100%;text-align:left;padding:12px 14px;border-radius:10px;border:1px solid #262e40;background:#0d1017;color:#f3f4f6;cursor:pointer;display:flex;flex-direction:column;gap:3px;transition:border-color .15s,background .15s}
  .org-btn:hover{border-color:#3b82f6;background:#151b28}
  .org-name{font-size:14px;font-weight:600}
  .org-meta{font-size:11.5px;color:#9ca3af;font-family:ui-monospace,SFMono-Regular,Menlo,monospace}
  .divider{display:flex;align-items:center;gap:10px;margin:20px 0 14px;color:#6b7280;font-size:12px}
  .divider::before,.divider::after{content:"";flex:1;height:1px;background:#1f2430}`,
		card: `<div class="card">
  <h1>Choose an organization</h1>
  <p class="sub">${opts.email ? `Signed in as ${escapeHtml(opts.email)}. ` : ""}Each organization runs its own isolated hotbed cluster.</p>
  ${errorLine(opts.error)}
  ${orgButtons || `<p class="sub">You don't belong to an organization yet. Create one below to start your cluster.</p>`}
  ${createSection}
  ${SIGN_OUT_FOOTER}
</div>`,
	});
}

export function renderSignupCodePage(opts: {
	email: string;
	error?: string;
	returnTo?: string;
}): string {
	return cardPage({
		title: "hotbed — sign-up code",
		maxWidth: 440,
		css: `
  input[name=code]{font-family:ui-monospace,SFMono-Regular,Menlo,monospace;letter-spacing:.04em}`,
		card: `<div class="card">
  <h1>Enter sign-up code</h1>
  <p class="sub">${opts.email ? `Signed in with Google as ${escapeHtml(opts.email)}. ` : ""}New hotbed accounts require a sign-up code to continue.</p>
  ${errorLine(opts.error)}
  <form method="POST" action="/auth/signup-code">
    ${returnToInput(opts.returnTo)}
    <input type="text" name="code" placeholder="Sign-up code" autocomplete="one-time-code" autocapitalize="characters" spellcheck="false" autofocus required />
    <button type="submit" class="primary">Continue</button>
  </form>
  ${SIGN_OUT_FOOTER}
</div>`,
	});
}
