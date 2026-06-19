"use strict";
// Second bundle reached from main.bundle.js. The self-import below proves the
// BFS dedupes already-seen URLs and never loops forever.
export default function () {
  return import("./foo.js");
}
