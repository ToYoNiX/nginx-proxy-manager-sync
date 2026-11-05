// npm-sync web UI. Plain JavaScript, no build step; all text is inserted
// with textContent, never as HTML.
"use strict";

const $ = (id) => document.getElementById(id);
let status = null;
let refreshTimer = null;

// ---- helpers ----

function el(tag, attrs, ...children) {
	const e = document.createElement(tag);
	for (const [k, v] of Object.entries(attrs || {})) {
		if (k === "class") e.className = v;
		else if (k === "title") e.title = v;
		else e.setAttribute(k, v);
	}
	for (const c of children) {
		if (c === null || c === undefined) continue;
		e.append(c instanceof Node ? c : String(c));
	}
	return e;
}

function badge(text, colour) {
	return el("span", { class: `badge bg-${colour}-lt` }, text);
}

const actionColours = {
	created: "green", updated: "azure", deleted: "red", unchanged: "secondary",
	skipped: "yellow", excluded: "purple", degraded: "orange", error: "red",
};

function actionBadge(o) {
	const b = badge(o.dryRun ? `would be ${o.action}` : o.action, o.failed ? "red" : actionColours[o.action] || "secondary");
	if (o.failed) b.title = "skipped for a reason that counts as an error";
	return b;
}

function fmtTime(s) {
	if (!s) return "—";
	const d = new Date(s);
	return isNaN(d) ? "—" : d.toLocaleString();
}

function fmtAgo(s) {
	if (!s) return "";
	const secs = Math.round((Date.now() - new Date(s)) / 1000);
	const abs = Math.abs(secs);
	const unit = abs < 90 ? [abs, "s"] : abs < 5400 ? [Math.round(abs / 60), "min"] : abs < 129600 ? [Math.round(abs / 3600), "h"] : [Math.round(abs / 86400), "d"];
	return secs >= 0 ? `${unit[0]} ${unit[1]} ago` : `in ${unit[0]} ${unit[1]}`;
}

function duration(a, b) {
	const ms = new Date(b) - new Date(a);
	return ms < 1000 ? `${ms} ms` : `${(ms / 1000).toFixed(1)} s`;
}

async function api(path, opts) {
	const res = await fetch(path, Object.assign({ headers: { Accept: "application/json" } }, opts));
	let body = null;
	try { body = await res.json(); } catch (_) { /* not JSON */ }
	if (!res.ok && res.status !== 409) throw new Error((body && body.message) || `${res.status} ${res.statusText}`);
	return { status: res.status, body };
}

function showError(msg) {
	const a = $("alert");
	a.textContent = msg || "";
	a.classList.toggle("d-none", !msg);
}

function backupSucceeded(b) {
	if (b.aborted || b.error) return false;
	return !(b.objects || []).some((o) => o.action === "error" || o.failed);
}

function counts(b) {
	const c = {};
	for (const o of b.objects || []) {
		const k = o.failed ? "error" : o.action;
		c[k] = (c[k] || 0) + 1;
	}
	return c;
}

// ---- dashboard ----

function renderDashboard() {
	const s = status;
	const last = s.lastRun;
	if (last) {
		const ok = last.backups && last.backups.length && last.backups.every(backupSucceeded) && !last.error;
		const parts = [fmtAgo(last.finished), " ", badge(ok ? "success" : "failed", ok ? "green" : "red")];
		if (last.dryRun) parts.push(" ", badge("dry run", "yellow"));
		if (last.forcePasswords) parts.push(" ", badge("passwords sent", "azure"));
		$("last-run").replaceChildren(...parts);
		$("last-run-detail").textContent = `${fmtTime(last.finished)} · trigger: ${last.trigger} · took ${duration(last.started, last.finished)}` +
			(last.error ? ` · ${last.error}` : "");
	} else {
		$("last-run").textContent = s.running ? "running…" : "no run yet";
		$("last-run-detail").textContent = "";
	}
	$("next-run").textContent = s.nextRun ? fmtAgo(s.nextRun) : "not scheduled";
	$("schedule").textContent = s.nextRun ? `${fmtTime(s.nextRun)} · cron "${s.cron}"` : "cron is empty; runs start from here or the API";
	$("sync-now").disabled = $("sync-force").disabled = s.running;

	const byURL = {};
	for (const b of (last && last.backups) || []) byURL[b.url] = b;
	const rows = s.instances.map((inst) => {
		const reach = inst.reachable ? badge("reachable", "green") : badge("unreachable", "red");
		if (inst.error) reach.title = inst.error;
		let result = "—", changes = "";
		if (inst.role === "master" && last) {
			result = last.error ? badge("error", "red") : badge("read", "green");
			if (last.error) result.title = last.error;
		}
		const b = byURL[inst.url];
		if (inst.role === "backup" && b) {
			if (b.aborted) {
				result = el("span", {}, badge("aborted", "red"), " ", el("span", { class: "text-secondary small" }, b.error));
			} else {
				result = backupSucceeded(b) ? badge("success", "green") : badge("errors", "red");
			}
			const c = counts(b);
			changes = el("span", { class: "counts" });
			for (const a of ["created", "updated", "deleted", "degraded", "skipped", "error"]) {
				if (c[a]) changes.append(badge(`${c[a]} ${a === "error" ? (c[a] > 1 ? "errors" : "error") : a}`, actionColours[a]));
			}
			if (!changes.childNodes.length) changes.append(el("span", { class: "text-secondary" }, "no changes"));
		}
		return el("tr", {},
			el("td", {}, inst.role === "master" ? badge("master", "blue") : badge("backup", "secondary")),
			el("td", { class: "domain-name" }, inst.url),
			el("td", {}, reach),
			el("td", {}, inst.version || "—"),
			el("td", {}, result),
			el("td", {}, changes));
	});
	$("instances").replaceChildren(...rows);
}

async function triggerSync(force) {
	showError("");
	try {
		const r = await api(`/api/v1/sync${force ? "?forcePasswords=true" : ""}`, { method: "POST" });
		if (r.status === 409) showError("A run is already in progress.");
	} catch (e) {
		showError(`Could not start a run: ${e.message}`);
	}
	refresh();
}

// ---- objects ----

function renderObjects() {
	const last = status.lastRun;
	const backups = (last && last.backups) || [];
	const sel = $("obj-backup");
	const current = sel.value;
	if (sel.options.length !== backups.length || [...sel.options].some((o, i) => o.value !== backups[i].url)) {
		sel.replaceChildren(...backups.map((b) => el("option", { value: b.url }, b.url)));
		if (current) sel.value = current;
	}
	const b = backups.find((x) => x.url === sel.value) || backups[0];
	const action = $("obj-action").value;
	const q = $("obj-search").value.trim().toLowerCase();
	const objs = ((b && b.objects) || []).filter((o) =>
		(!action || o.action === action) &&
		(!q || `${o.type} ${o.name} ${o.message || ""}`.toLowerCase().includes(q)));
	const order = { certificate: 0, "access list": 1 };
	objs.sort((x, y) => (order[x.type] ?? 2) - (order[y.type] ?? 2) || x.type.localeCompare(y.type) || x.name.localeCompare(y.name));
	$("objects").replaceChildren(...objs.map((o) => el("tr", {},
		el("td", { class: "text-nowrap" }, o.type),
		el("td", { class: o.type === "certificate" || o.type === "access list" ? "" : "domain-name" }, o.name),
		el("td", {}, actionBadge(o)),
		el("td", { class: "msg text-secondary" }, o.message || ""),
		el("td", {}, o.expiresOn ? fmtTime(o.expiresOn) : ""),
		el("td", {}, o.lastCopied ? fmtTime(o.lastCopied) : ""))));
	$("objects-empty").hidden = objs.length > 0;
}

// ---- logs ----

const levelColours = { debug: "secondary", info: "green", warn: "yellow", error: "red" };

async function renderLogs() {
	const r = await api(`/api/v1/logs?level=${encodeURIComponent($("log-level").value)}`);
	const entries = r.body.entries || [];
	const box = document.querySelector(".logs");
	const atBottom = box.scrollHeight - box.scrollTop - box.clientHeight < 40;
	$("logs").replaceChildren(...entries.map((e) => el("tr", {},
		el("td", { class: "log-time" }, new Date(e.time).toLocaleString()),
		el("td", { class: "log-level" }, badge(e.level, levelColours[e.level] || "secondary")),
		el("td", {}, e.line))));
	$("logs-empty").hidden = entries.length > 0;
	if (atBottom) box.scrollTop = box.scrollHeight;
}

async function clearLogs() {
	if (!confirm("Clear the log buffer?")) return;
	await api("/api/v1/logs/clear", { method: "POST" });
	renderLogs();
}

// ---- routing and refresh ----

function page() {
	const p = location.hash.replace("#", "");
	return ["dashboard", "objects", "logs"].includes(p) ? p : "dashboard";
}

async function refresh() {
	clearTimeout(refreshTimer);
	try {
		status = (await api("/api/v1/status")).body;
		$("app-version").textContent = status.version;
		const rs = $("run-state");
		rs.className = status.running ? "badge bg-azure-lt" : status.dryRun ? "badge bg-yellow-lt" : "badge d-none";
		rs.textContent = status.running ? "sync running…" : status.dryRun ? "dry run mode" : "";
		const p = page();
		if (p === "dashboard") renderDashboard();
		if (p === "objects") renderObjects();
		if (p === "logs") await renderLogs();
		if ($("alert").textContent.startsWith("Cannot reach")) showError("");
	} catch (e) {
		showError(`Cannot reach npm-sync: ${e.message}`);
	}
	refreshTimer = setTimeout(refresh, status && status.running ? 2000 : 10000);
}

function route() {
	const p = page();
	for (const s of ["dashboard", "objects", "logs"]) $(`page-${s}`).hidden = s !== p;
	for (const a of document.querySelectorAll("#nav .nav-link")) {
		a.parentElement.classList.toggle("active", a.dataset.page === p);
	}
	refresh();
}

$("sync-now").addEventListener("click", () => triggerSync(false));
$("sync-force").addEventListener("click", () => triggerSync(true));
$("log-clear").addEventListener("click", clearLogs);
$("log-level").addEventListener("change", renderLogs);
for (const id of ["obj-backup", "obj-action"]) $(id).addEventListener("change", renderObjects);
$("obj-search").addEventListener("input", renderObjects);
$("theme-toggle").addEventListener("click", () => {
	const dark = document.documentElement.getAttribute("data-bs-theme") !== "dark";
	document.documentElement.setAttribute("data-bs-theme", dark ? "dark" : "light");
	localStorage.setItem("tabler-theme", dark ? "dark" : "light");
});
window.addEventListener("hashchange", route);
route();
