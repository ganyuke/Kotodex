// Confirm before destructive forms, submit filter dropdowns on change.
document.addEventListener("submit", (e) => {
  const msg = e.target.dataset.confirm;
  if (msg && !confirm(msg)) e.preventDefault();
});
document.addEventListener("change", (e) => {
  if (e.target.matches("select[data-autosubmit]")) e.target.form.submit();
});

// Refresh the [data-live] part of the page in place: every 2 seconds while
// something is downloading or being checked, every 15 seconds otherwise.
(() => {
  const live = () => document.querySelector("[data-live]");
  if (!live()) return;
  const delay = () => (live()?.hasAttribute("data-busy") ? 2000 : 15000);
  const tick = async () => {
    if (!document.hidden) {
      try {
        const res = await fetch(location.href, { cache: "no-store" });
        if (res.ok && !res.redirected) {
          const doc = new DOMParser().parseFromString(await res.text(), "text/html");
          const fresh = doc.querySelector("[data-live]");
          if (fresh && live()) live().replaceWith(fresh);
        }
      } catch {}
    }
    setTimeout(tick, delay());
  };
  setTimeout(tick, delay());
})();
