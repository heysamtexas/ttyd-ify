// Astigmatism mode: a low-halation look set in Intel One Mono, at a size you pick (#140).
//
// Loaded as a blocking <script> in the <head> of every page, like sub.js, so the class and the
// ground colour are on <html> before first paint.
//
// Why it looks the way it does. The default grid is xterm's own theme, pure #fff on pure #000,
// 21:1: the worst case for an astigmatic eye, where bright strokes on a dark field bleed and
// ghost ("halation"). So contrast comes *down* -- off-white on dark grey, ~11:1, still well
// above WCAG AAA's 7:1 -- and weight goes up instead: Medium for text, Bold for bold. Intel One
// Mono was designed with low-vision developers. The terminal page reads the size and palette
// below for its xterm options; the other pages only need the face and the ground.
//
// The switch cycles off -> 14px -> 18px -> 24px. Three sizes rather than one because they were
// tried live and the right one depends on the screen and on which part of a progressive lens
// is doing the reading. 14px keeps the default size, but the taller line and the different
// face still change how many rows and columns fit.
//
// Per browser (localStorage), shared by every page on this origin; the "storage" event carries
// a change to other open tabs. The iOS app renders natively and never loads this.
(function () {
  "use strict";
  const KEY = "wt.astig";
  const FAMILY = '"Intel One Mono"';
  const SIZES = [14, 18, 24];
  const root = document.documentElement;

  // Softened so no colour glares. Every entry but black is >= 4.5:1 on BG (brightBlack, the
  // "dim/comment" colour, is the tightest at ~4.8) and FG is >= 7:1; picker_test.go checks
  // both. black is exempt -- programs paint it as a background -- and minimumContrastRatio
  // lifts it, along with every 256-colour and truecolor foreground painted past this theme.
  const BG = "#1e1e1e", FG = "#d4d4d4";
  const PALETTE = {
    black: "#2e2e2e", red: "#e8837e", green: "#9ccc8a", yellow: "#d9c27a",
    blue: "#8fb2e8", magenta: "#c7a0e0", cyan: "#7fc8c8", white: "#c8c8c8",
    brightBlack: "#8a8a8a", brightRed: "#f0a09b", brightGreen: "#b5dba5",
    brightYellow: "#e8d59a", brightBlue: "#a9c7f0", brightMagenta: "#d8bdea",
    brightCyan: "#a0dada", brightWhite: "#e0e0e0",
    cursor: "#e8b86b", selectionBackground: "#3f4f66",
  };

  // The two weights the mode uses; relative URLs, like every other asset (#57).
  const style = document.createElement("style");
  style.textContent =
    "@font-face { font-family: " + FAMILY + "; font-weight: 500; font-display: block;" +
    " src: url(vendor/IntelOneMono-Medium.woff2) format(\"woff2\"); }" +
    "@font-face { font-family: " + FAMILY + "; font-weight: 700; font-display: block;" +
    " src: url(vendor/IntelOneMono-Bold.woff2) format(\"woff2\"); }" +
    "html.astig { --astig-font: " + FAMILY + ", ui-monospace, monospace;" +
    " --astig-bg: " + BG + "; --astig-fg: " + FG + "; --astig-muted: #a8a8a8; }";
  document.head.appendChild(style);

  // 0 is off.
  function read() {
    try {
      const v = Number(localStorage.getItem(KEY));
      return SIZES.includes(v) ? v : 0;
    } catch (_) { return 0; }
  }

  let size = 0;
  function apply(next) {
    size = next;
    root.classList.toggle("astig", size > 0);
    // Start fetching the faces now rather than when first used, so the terminal's font swap
    // lands in the first frames instead of after the grid is already drawn in the fallback.
    if (size && document.fonts) {
      for (const w of [500, 700]) document.fonts.load(w + " " + size + "px " + FAMILY).catch(() => {});
    }
    document.dispatchEvent(new CustomEvent("wt-astig", { detail: size }));
  }

  apply(read());
  window.addEventListener("storage", (ev) => { if (ev.key === KEY) apply(read()); });

  window.wtAstig = {
    FAMILY, BG, FG, PALETTE,
    size: () => size,
    cycle() {
      const next = [0, ...SIZES][([0, ...SIZES].indexOf(size) + 1) % (SIZES.length + 1)];
      try { localStorage.setItem(KEY, String(next)); } catch (_) { /* still applies to this page */ }
      apply(next);
    },
  };
})();
