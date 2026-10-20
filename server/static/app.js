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

// The file dialog can still be switched to all files. Refuse the wrong type
// before the form is submitted.
document.addEventListener("change", (e) => {
  const input = e.target;
  if (!(input instanceof HTMLInputElement) || input.type !== "file" || input.name !== "archive") return;
  const file = input.files?.[0];
  input.setCustomValidity(file && !/\.(cbz|zip)$/i.test(file.name) ? "Choose a CBZ or ZIP file." : "");
});

// Page uploads: files or a folder, whichever was chosen last, sent with a
// progress bar. The server answers with where to go next.
document.addEventListener("change", (e) => {
  const input = e.target;
  const form = input.closest?.("form[data-upload]");
  if (!form || input.type !== "file") return;
  form.querySelectorAll("input[type=file]").forEach((other) => {
    if (other !== input) other.value = "";
  });
  const n = input.files.length;
  form.querySelector("[data-upload-chosen]").textContent =
    n === 0 ? "No pages chosen" : n === 1 ? input.files[0].name : `${n} files chosen`;
});

let uploading = false;
window.addEventListener("beforeunload", (e) => {
  if (uploading) e.preventDefault();
});

document.addEventListener("submit", (e) => {
  const form = e.target.closest?.("form[data-upload]");
  if (!form) return;
  e.preventDefault();
  if (uploading) return;
  const bar = form.querySelector("progress");
  const status = form.querySelector("[data-upload-status]");
  const button = form.querySelector("button");
  const show = (text, error) => {
    status.hidden = false;
    status.className = error ? "error" : "muted";
    status.textContent = text;
  };
  if (![...form.querySelectorAll("input[type=file]")].some((i) => i.files.length)) {
    show("Choose the pages first.", true);
    return;
  }
  const done = (text) => {
    uploading = false;
    button.disabled = false;
    bar.hidden = true;
    show(text, true);
  };
  const xhr = new XMLHttpRequest();
  xhr.open("POST", form.action);
  xhr.setRequestHeader("Kotodex-Upload", "1");
  xhr.upload.addEventListener("progress", (p) => {
    if (!p.lengthComputable) return;
    bar.max = p.total;
    bar.value = p.loaded;
    show(`Uploading, ${Math.floor((p.loaded * 100) / p.total)}% done`);
  });
  xhr.upload.addEventListener("load", () => show("Saving pages"));
  xhr.addEventListener("load", () => {
    const next = xhr.getResponseHeader("Kotodex-Location");
    if (xhr.status === 204 && next) {
      uploading = false;
      location.assign(next);
      return;
    }
    // A reverse proxy's size limit answers with its own page.
    if (xhr.status === 413) {
      done("The upload is over the server's size limit.");
      return;
    }
    const page = new DOMParser().parseFromString(xhr.responseText, "text/html");
    done(page.querySelector("main h1")?.textContent || "Upload failed.");
  });
  xhr.addEventListener("error", () => done("Upload failed. Check the connection and try again."));
  uploading = true;
  button.disabled = true;
  bar.hidden = false;
  bar.value = 0;
  show("Uploading, 0% done");
  xhr.send(new FormData(form));
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

// Dismiss the source menu without changing the applied chapter selection.
document.addEventListener("click", (event) => {
  const menu = document.querySelector(".source-filter[open]");
  if (menu && !menu.contains(event.target)) menu.open = false;
});
document.addEventListener("keydown", (event) => {
  const menu = document.querySelector(".source-filter[open]");
  if (event.key === "Escape" && menu) {
    menu.open = false;
    menu.querySelector("summary").focus();
  }
});

// Reordering changes every source's priority, independently of its checkbox.
document.addEventListener("click", (event) => {
  const move = event.target.closest("[data-source-move]");
  if (!move) return;
  const row = move.closest("[data-source-row]");
  const up = move.dataset.sourceMove === "-1";
  const adjacent = up ? row.previousElementSibling : row.nextElementSibling;
  if (!adjacent) return;
  up ? adjacent.before(row) : adjacent.after(row);
  const rows = [...row.parentElement.children];
  rows.forEach((item, index) => {
    item.querySelector(".source-rank").textContent = index + 1;
    item.querySelector('[data-source-move="-1"]').disabled = index === 0;
    item.querySelector('[data-source-move="1"]').disabled = index === rows.length - 1;
  });
  if (move.disabled) row.querySelector(`[data-source-move="${up ? 1 : -1}"]`).focus();
});
