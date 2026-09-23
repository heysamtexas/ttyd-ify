// Submarine mode: red on black, for working in the dark without losing night vision.
//
// Loaded as a blocking <script> in the <head> of every page, so the class is on <html> before
// first paint -- a page that flashed its daylight colours before turning red would defeat the
// point at 2am.
//
// A colour-matrix filter over the whole page, not a red palette. A palette only reaches colours
// this page chooses; the terminal also shows whatever the program in it paints -- 256-colour and
// truecolor escapes never pass through xterm's theme -- plus the favicon-matched status hexes
// and the banner. The matrix below sends every pixel's channel average to red and zeroes green
// and blue, so nothing on the page can emit anything but red. The average rather than luminance
// weighting, because luminance puts blue text at ~7% and it would vanish; K below 1 caps the
// brightest white. picker_test.go pins the zero rows.
//
// The setting is per browser (localStorage) and shared by every page on this origin; the
// "storage" event carries a toggle to other open tabs. It cannot reach the browser's own
// chrome, and the iOS app renders natively and never loads this.
(function () {
  "use strict";
  const KEY = "wt.sub";
  const root = document.documentElement;

  const svg = document.createElement("div");
  svg.innerHTML =
    '<svg width="0" height="0" style="position:absolute" aria-hidden="true">' +
    '<filter id="wt-sub" color-interpolation-filters="sRGB">' +
    '<feColorMatrix type="matrix" values="' +
    "0.2667 0.2667 0.2667 0 0 " +
    "0 0 0 0 0 " +
    "0 0 0 0 0 " +
    "0 0 0 1 0" +
    '"/></filter></svg>';
  // Before <body> exists, so it goes on <html>; the parser appends <body> after it.
  root.appendChild(svg.firstChild);

  const style = document.createElement("style");
  // The canvas outside <html>'s box is not filtered, so <html> has to cover the viewport and
  // be black itself; color-scheme keeps form controls and scrollbars dark. A <dialog> opened
  // with showModal() renders in the top layer, outside <html>'s filter, so it needs its own.
  style.textContent =
    "html.sub { background: #000; min-height: 100%; color-scheme: dark; filter: url(#wt-sub); }" +
    "html.sub dialog { filter: url(#wt-sub); }" +
    "html.sub dialog::backdrop { background: rgba(0,0,0,.8); }";
  document.head.appendChild(style);

  function read() {
    try { return localStorage.getItem(KEY) === "1"; } catch (_) { return false; }
  }
  function apply(on) {
    root.classList.toggle("sub", on);
    document.dispatchEvent(new CustomEvent("wt-sub", { detail: on }));
  }

  apply(read());
  window.addEventListener("storage", (ev) => { if (ev.key === KEY) apply(read()); });

  window.wtSub = {
    on: () => root.classList.contains("sub"),
    toggle() {
      const on = !this.on();
      try { localStorage.setItem(KEY, on ? "1" : "0"); } catch (_) { /* still toggles this page */ }
      apply(on);
    },
  };
})();
