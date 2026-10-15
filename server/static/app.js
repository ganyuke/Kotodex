// Confirm before destructive forms, submit filter dropdowns on change.
document.addEventListener("submit", (e) => {
  const msg = e.target.dataset.confirm;
  if (msg && !confirm(msg)) e.preventDefault();
});
document.addEventListener("change", (e) => {
  if (e.target.matches("select[data-autosubmit]")) e.target.form.submit();
});

// Show the address this page was opened at. The server can only guess it
// from proxy headers.
document.querySelectorAll("[data-origin]").forEach((el) => (el.textContent = location.origin));

// Keep the [data-live] part of the page up to date. The server sends a new
// copy over an event stream whenever it changes. The stream is closed while
// the tab is hidden, so background tabs don't each hold a connection open.
//
// The browser reconnects dropped streams by itself, but gives up for good
// when a reconnect gets an error page (a proxy's 502 while the server
// restarts). Then it's reopened here, waiting twice as long after each
// failure, up to a minute. Signing out ends it for good.
(() => {
  if (!document.querySelector("[data-live]")) return;
  const maxDelay = 60000;
  let events = null;
  let retry = null;
  let delay = 1000;
  let signedOut = false;
  const close = () => {
    events?.close();
    events = null;
    clearTimeout(retry);
    retry = null;
  };
  const open = () => {
    close();
    events = new EventSource(location.href);
    events.onopen = () => (delay = 1000);
    events.onmessage = (e) => {
      const fresh = new DOMParser().parseFromString(e.data, "text/html").querySelector("[data-live]");
      const live = document.querySelector("[data-live]");
      if (fresh && live) live.replaceWith(fresh);
    };
    events.addEventListener("signedout", () => {
      signedOut = true;
      close();
    });
    events.onerror = () => {
      if (events?.readyState !== EventSource.CLOSED) return; // the browser is retrying
      events = null;
      if (signedOut || document.hidden) return;
      // Random spread so tabs don't all come back at the same moment.
      retry = setTimeout(open, delay / 2 + Math.random() * (delay / 2));
      delay = Math.min(delay * 2, maxDelay);
    };
  };
  document.addEventListener("visibilitychange", () => {
    if (document.hidden) close();
    else if (!events && !signedOut) open();
  });
  if (!document.hidden) open();
})();
