// webui/sanitize.js 回归测试（node + jsdom）。真实链路：marked 渲染 → sanitize → 断言。
import { createRequire } from 'module';
import fs from 'fs';
import path from 'path';
import { fileURLToPath } from 'url';
const require = createRequire(import.meta.url);
const here = path.dirname(fileURLToPath(import.meta.url));
const { JSDOM } = require(process.env.OKHUMAN_JSDOM || 'jsdom');
const dom = new JSDOM('<!doctype html><html><body></body></html>');
globalThis.window = dom.window;
globalThis.DOMParser = dom.window.DOMParser;
(0, eval)(fs.readFileSync(path.join(here, 'sanitize.js'), 'utf8'));
const S = globalThis.sanitizeHtml || dom.window.sanitizeHtml;
const marked = require(path.join(here, 'marked.min.js'));
const parse = marked.parse || marked.marked;
const md = (s) => S(parse(s, { gfm: true, breaks: true }));

const evil = [
  '\n\n<img src=x onerror=alert(1)>\n\n正文\n',
  '\n\n<svg onload=alert(1)></svg>\n\n正文\n',
  '\n\n<iframe src="data:text/html,<script>alert(1)<\/script>"></iframe>\n',
  '\n\n<details><summary onclick="alert(1)">x</summary>y</details>\n',
  '[点我](javascript:alert(1))\n',
  '<a href="java\u0000script:alert(1)">x</a>',
  '<img src="/x" onerror="alert(1)">',
  '<form action="/enqueue"><input name="message"><button>点我</button></form>',
  '<p style="background:url(javascript:alert(1))">x</p>',
  '<object data="data:text/html;base64,PHNjcmlwdD5hbGVydCgxKTwvc2NyaXB0Pg=="></object>',
  '<a href="//evil.example.com/x">协议相对</a>',
  '<img src="\\tahttps://ok/x" onerror=alert(1)>',
  '<math><mi xlink:href="javascript:alert(1)">x</mi></math>',
];
const BAD = /(on[a-z]+\s*=|javascript:|vbscript:|<script|<iframe|<svg|<form|<object|<embed|<style|<link|<meta|<math|srcdoc|<input|<button)/i;
let fail = 0;
for (const p of evil) {
  const out = md(p);
  if (BAD.test(out)) { console.log('  漏过：' + JSON.stringify(p) + '\n        → ' + out); fail++; }
}
const good = [
  ['**粗** 和 `code`', /<strong>粗<\/strong>/],
  ['[链接](https://example.com/a?b=1)', /<a href="https:\/\/example\.com\/a\?b=1">/],
  ['![图](https://example.com/i.png)', /<img src="https:\/\/example\.com\/i\.png"/],
  ['自动链接 <https://example.com>', /<a href="https:\/\/example\.com">/],
  ['\n\n| a | b |\n|---|---|\n| 1 | 2 |\n\n', /<table>/],
  ['- 列表项', /<ul>/],
  ['<b>粗体 HTML</b>', /<b>粗体 HTML<\/b>/],
];
for (const [src, re] of good) {
  const out = md(src);
  if (!re.test(out)) { console.log('  误伤：' + JSON.stringify(src) + '\n        → ' + out); fail++; }
}
console.log(fail ? `FAIL：${fail} 条不通过` : `sanitize 全部通过（${evil.length} 个攻击样本 + ${good.length} 个正常样本）`);
process.exit(fail ? 1 : 0);
