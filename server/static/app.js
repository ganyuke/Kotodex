// Forms, links, confirm dialogs, and live sections are htmx (see layout.html).
// htmx also closes live streams in background tabs and reconnects dropped
// ones.

// Show the address this page was opened at. The server can only guess it
// from proxy headers.
htmx.onLoad((el) => {
  el.querySelectorAll("[data-origin]").forEach((o) => (o.textContent = location.origin));
  // Kotatsu's sign-in only takes an address ending in 2 to 6 letters. Use
  // this site's name when it fits, otherwise a name that can never be real.
  const host = /^[a-z0-9.-]+\.[a-z]{2,6}$/i.test(location.hostname) ? location.hostname : "kotodex.home.arpa";
  el.querySelectorAll("[data-sync-email]").forEach((e) => (e.textContent = e.dataset.syncEmail + "@" + host));
});

// The server ends the live stream with an "end" event when the page is gone
// or the user is signed out. Reload to show that.
document.addEventListener("htmx:sse:close", (e) => {
  if (e.detail.reason === "message") location.reload();
});

// Chapter table columns: hide, show, and reorder. Saved in a cookie, so the
// server draws the table the same way, live updates included.
const colsCookie = "chapter_columns";

function saveColumns(menu) {
  const order = [...menu.querySelectorAll("[data-col-row]")].map(
    (row) => (row.querySelector("[data-col-toggle]").checked ? "" : "-") + row.dataset.colRow,
  );
  document.cookie = `${colsCookie}=${order.join(".")}; path=/; max-age=31536000; samesite=lax`;
  try {
    sessionStorage.setItem("colmenu-open", "1");
  } catch {}
  location.reload();
}

document.addEventListener("change", (e) => {
  if (e.target.matches("[data-col-toggle]")) saveColumns(e.target.closest(".colmenu"));
});

document.addEventListener("click", (e) => {
  const move = e.target.closest("[data-col-move]");
  if (move) {
    const row = move.closest("[data-col-row]");
    const left = move.dataset.colMove === "-1";
    const next = left ? row.previousElementSibling : row.nextElementSibling;
    if (next && next.matches("[data-col-row]")) {
      left ? next.before(row) : next.after(row);
      saveColumns(row.closest(".colmenu"));
    }
    return;
  }
  if (e.target.closest("[data-col-reset]")) {
    document.cookie = `${colsCookie}=; path=/; max-age=0; samesite=lax`;
    location.reload();
  }
});

// Drag a column header onto another to move it there. Dropping on the right
// half of a header puts it after that one.
let draggedCol = null;
document.addEventListener("dragstart", (e) => {
  const th = e.target.closest?.("th[data-col]");
  if (!th) return;
  draggedCol = th.dataset.col;
  e.dataTransfer.effectAllowed = "move";
  e.dataTransfer.setData("text/plain", draggedCol);
});
document.addEventListener("dragover", (e) => {
  if (draggedCol && e.target.closest?.("th[data-col]")) e.preventDefault();
});
document.addEventListener("drop", (e) => {
  const th = e.target.closest?.("th[data-col]");
  const menu = document.querySelector(".colmenu");
  if (!draggedCol || !th || !menu || th.dataset.col === draggedCol) return;
  e.preventDefault();
  const from = menu.querySelector(`[data-col-row="${draggedCol}"]`);
  const to = menu.querySelector(`[data-col-row="${th.dataset.col}"]`);
  const box = th.getBoundingClientRect();
  e.clientX > box.left + box.width / 2 ? to.after(from) : to.before(from);
  draggedCol = null;
  saveColumns(menu);
});
document.addEventListener("dragend", () => (draggedCol = null));

// Keep the Columns menu open after the reload a change makes.
htmx.onLoad((el) => {
  try {
    if (!sessionStorage.getItem("colmenu-open")) return;
    const menu = el.querySelector?.(".colmenu");
    if (menu) {
      sessionStorage.removeItem("colmenu-open");
      menu.open = true;
    }
  } catch {}
});
