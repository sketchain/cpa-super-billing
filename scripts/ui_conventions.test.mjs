import assert from "node:assert/strict";
import fs from "node:fs";
import test from "node:test";
import { parse } from "@babel/parser";

// Upstream v1.4.18 to v1.4.20 replaced the native select styling, the .notice show/err/ok classes and the
// "err"/"ok" notification kinds. Code written against the old API renders unstyled or throws, so it is
// rejected here instead of in a browser.
const root = new URL("../internal/plugin/web/", import.meta.url);
const files = fs.readdirSync(root).filter((name) => /\.(js|css|html)$/.test(name) && name !== "i18n.js").sort();
const read = (name) => fs.readFileSync(new URL(name, root), "utf8");
const KINDS = new Set(["success", "info", "warning", "error"]);
// Known, reviewed exceptions as "file:line": reason. Keep empty unless a use is deliberate.
const ALLOWED = {};

function walk(node, visit) {
  if (!node || typeof node.type !== "string") return;
  visit(node);
  for (const [key, value] of Object.entries(node)) {
    if (key === "loc" || key === "start" || key === "end") continue;
    if (Array.isArray(value)) for (const child of value) walk(child, visit);
    else if (value && typeof value === "object") walk(value, visit);
  }
}

function findings() {
  const found = [];
  const report = (file, line, message) => { if (!ALLOWED[file + ":" + line]) found.push(`${file}:${line}: ${message}`); };
  for (const file of files) {
    const text = read(file);
    if (!file.endsWith(".js")) {
      text.split("\n").forEach((line, index) => {
        if (/native-select/.test(line)) report(file, index + 1, "native-select is no longer styled; use ui-select");
        if (file.endsWith(".html") && /<select\b/.test(line)) report(file, index + 1, "native <select> is no longer styled; use <ui-select>");
      });
      continue;
    }
    const ast = parse(text, { sourceType: "script", errorRecovery: false });
    walk(ast.program, (node) => {
      const line = node.loc.start.line;
      if (node.type === "CallExpression" && node.callee.type === "Identifier") {
        const [first, second] = node.arguments;
        if (["notify", "notice"].includes(node.callee.name) && second?.type === "StringLiteral" && !KINDS.has(second.value))
          report(file, line, `${node.callee.name}() kind "${second.value}" is not one of ${[...KINDS].join(", ")}`);
        if (node.callee.name === "el" && first?.type === "StringLiteral" && first.value === "select")
          report(file, line, 'el("select") is no longer styled; use el("ui-select")');
      }
      const value = node.type === "StringLiteral" ? node.value : node.type === "TemplateElement" ? node.value.cooked : null;
      if (value == null) return;
      if (/(^|\s)notice(\s|$)/.test(value) && /(^|\s)(show|err|ok)(\s|$)/.test(value))
        report(file, line, `class "${value}" uses the removed .notice show/err/ok styles; use notice(message, kind)`);
      if (/native-select/.test(value)) report(file, line, "native-select is no longer styled; use ui-select");
    });
  }
  return found;
}

test("web UI uses the current notice, notification and select APIs", () => {
  assert.deepEqual(findings(), []);
});
