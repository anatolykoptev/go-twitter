"use strict";
// Trimmed webpack runtime chunk. The dynamic import edge below is what
// WalkImports BFS-follows to reach foo.js.
(self.webpackChunk = self.webpackChunk || []).push([[42], {
  1001: function (e, t, n) {
    return import("./foo.js");
  }
}]);
