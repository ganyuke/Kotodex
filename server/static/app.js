// Forms, links, confirm dialogs, and live sections are htmx (see layout.html).
// htmx also closes live streams in background tabs and reconnects dropped
// ones.

// Show the address this page was opened at. The server can only guess it
// from proxy headers.
htmx.onLoad((el) => {
  el.querySelectorAll("[data-origin]").forEach((o) => (o.textContent = location.origin));
});

// The server ends the live stream with an "end" event when the page is gone
// or the user is signed out. Reload to show that.
document.addEventListener("htmx:sse:close", (e) => {
  if (e.detail.reason === "message") location.reload();
});
