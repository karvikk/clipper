import { readFileSync } from "node:fs";

for (const file of ["ui.html", "installer.html"]) {
  const html = readFileSync(file, "utf8");
  const start = html.indexOf("<script>") + "<script>".length;
  const end = html.indexOf("</script>", start);
  if (start < "<script>".length || end < start) {
    throw new Error(`Missing inline script in ${file}`);
  }
  new Function(html.slice(start, end));
  console.log(`${file} JavaScript syntax OK`);
}
