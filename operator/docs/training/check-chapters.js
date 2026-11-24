#!/usr/bin/env node
// Checks the chapter files before they are handed back: every file listed in
// chapters/index.js loads, registers one chapter of the shape chapter-schema.js
// describes, and names only example manifests that exist. The example
// manifests themselves are validated against the CRDs by the support tool's
// Go test, which this script does not repeat.
//
//   node check-chapters.js            # from this directory; exit 1 on a problem
'use strict';
const fs = require('fs');
const path = require('path');
const vm = require('vm');

const here = __dirname;
const schema = require(path.join(here, 'chapter-schema.js'));

const indexSrc = fs.readFileSync(path.join(here, 'chapters', 'index.js'), 'utf8');
const indexCtx = {};
vm.runInNewContext(indexSrc + '\nthis.CHAPTER_FILES = CHAPTER_FILES;', indexCtx);
const files = indexCtx.CHAPTER_FILES;

const problems = [];
const chapters = [];
for (const f of files) {
  const p = path.join(here, 'chapters', f);
  if (!fs.existsSync(p)) { problems.push(`${f}: listed in chapters/index.js but missing`); continue; }
  const registered = [];
  const ctx = { TRAINING: { register: ch => registered.push(ch) } };
  try {
    vm.runInNewContext(fs.readFileSync(p, 'utf8'), ctx, { filename: f });
  } catch (e) {
    problems.push(`${f}: ${e.message}`);
    continue;
  }
  if (registered.length !== 1) { problems.push(`${f}: registers ${registered.length} chapters, expected one`); continue; }
  const ch = registered[0];
  chapters.push(ch);
  for (const msg of schema.check(ch)) problems.push(`${f}: ${msg}`);
  for (const s of ch.sections || []) for (const ex of s.examples || []) {
    const p = path.join(here, ex.file);
    if (!fs.existsSync(p)) { problems.push(`${f}: example ${ex.file} does not exist`); continue; }
    // A note is drawn beside the first unmarked line containing its anchor, so
    // an anchor no line contains is a note that is never placed.
    const lines = fs.readFileSync(p, 'utf8').split('\n');
    const marked = new Set();
    for (const n of ex.notes || []) {
      const at = lines.findIndex((l, i) => !marked.has(i) && l.includes(n.at));
      if (at < 0) problems.push(`${f}: note "${n.at}" matches no unmarked line of ${ex.file}`);
      else marked.add(at);
    }
  }
}
for (const d of fs.existsSync(path.join(here, 'chapters')) ? fs.readdirSync(path.join(here, 'chapters')) : []) {
  if (d.endsWith('.js') && d !== 'index.js' && !files.includes(d)) problems.push(`chapters/${d}: not listed in chapters/index.js`);
}
const ids = chapters.map(c => c.id);
if (new Set(ids).size !== ids.length) problems.push('two chapters share an id');
// An example directory without a chapter is a leftover.
const exDir = path.join(here, 'examples');
for (const d of fs.existsSync(exDir) ? fs.readdirSync(exDir) : []) {
  if (!ids.includes(d)) problems.push(`examples/${d}: no chapter with that id`);
}

if (problems.length) {
  console.error(problems.join('\n'));
  process.exit(1);
}
const q = chapters.reduce((s, c) => s + c.questions.length, 0);
const t = chapters.reduce((s, c) => s + c.tasks.length, 0);
console.log(`${chapters.length} chapters, ${q} questions, ${t} manifest tasks: no problems`);
