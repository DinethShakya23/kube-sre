const assert = require("assert");
const { render } = require("./static/md.js");

const xss = render('<img src=x onerror=alert(1)> <script>alert(1)</script>');
assert(!/<img|<script/.test(xss), xss);

const code = render("```\n<b>hi</b>\n```");
assert(code.includes("&lt;b&gt;hi&lt;/b&gt;") && !code.includes("<b>"), code);

const link = render("[x](javascript:alert(1)) and [ok](https://example.com/a)");
assert(!link.includes('href="javascript'), link);
assert(link.includes('href="https://example.com/a"'), link);

const quote = render('[a](https://e.com/"onmouseover="x)');
assert(!/onmouseover="/.test(quote.replace(/&quot;/g, "")) || !quote.includes(' onmouseover='), quote);

assert(render("# Title").startsWith("<h2>Title</h2>"));
assert(render("- a\n- b").includes("<ul><li>a</li><li>b</li></ul>"));
assert(render("**bold** `c<d`").includes("<strong>bold</strong> <code>c&lt;d</code>"));
assert(render("| a | b |\n|---|---|\n| 1 | 2 |").includes("<td>1</td>"));
assert(render("") === "" && render(null) === "");
assert(!render("`x` %%KSCODE9%%").includes("undefined"));
console.log("md ok");
