// Applies the stored (or system) theme before first paint. Loaded as a
// same-origin classic script from index.html because the gateway CSP
// (script-src 'self') blocks inline scripts.
(() => {
  try {
    const stored = localStorage.getItem("gpuardian-theme");
    const dark = stored === "dark" || (stored !== "light" && matchMedia("(prefers-color-scheme: dark)").matches);
    document.documentElement.classList.toggle("dark", dark);
  } catch {
    document.documentElement.classList.toggle("dark", matchMedia("(prefers-color-scheme: dark)").matches);
  }
})();
