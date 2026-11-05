// Applies the saved light/dark theme before the page renders (same storage
// key as Nginx Proxy Manager, so both follow the same choice in a browser).
(function () {
	var saved = localStorage.getItem("tabler-theme");
	var dark = saved ? saved === "dark" : window.matchMedia("(prefers-color-scheme: dark)").matches;
	document.documentElement.setAttribute("data-bs-theme", dark ? "dark" : "light");
})();
