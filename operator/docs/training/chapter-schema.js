// The shape of a training chapter, and the check that a chapter file has it.
//
// It is one file loaded by two runtimes: chapter.html runs it in the browser and
// shows what it finds at the top of the page, and check-chapters.js runs it
// under node before a chapter is handed back. One implementation, so the two
// cannot disagree about what a chapter is.
//
// A chapter is registered by its file as TRAINING.register({...}) with:
//
//   id        kebab-case, the chapter's URL (chapter.html?c=<id>) and its key in
//             the results store
//   title     one line
//   summary   one or two sentences for the index page
//   goal      what the learner can do after the chapter, one or two sentences
//   sections  the lesson, in order; each { heading, body: [item...], examples?: [...] }
//             where an item is a paragraph string, { list: [...] }, { steps: [...] },
//             { code, title? }, { table: { head, rows } }, { callout, kind? }, or
//             { pitfalls: [{ symptom, cause, fix }] }, and an example is
//             { title, file, notes: [{ at, text }] } with file under examples/<id>/
//             and at a substring of the line the note is about
//   questions at least 25, each { topic, cat, text, options: [5 strings],
//             correct: [indices], explain }, cat one of concept, field, action;
//             a test draws 15, spread across the topics
//   tasks     two to four manifest tasks, each { title, scenario, yaml, blanks,
//             explain }, where yaml carries {{name}} placeholders and blanks maps
//             each name to { accept: [...], hint? }; an accept entry is the exact
//             text or a /regex/
//
// Inline `code` and **bold** are written with backticks and double asterisks in
// every prose string, and [text](relative-or-https-url) makes a link.
(function (root, factory) {
  if (typeof module !== 'undefined' && module.exports) module.exports = factory();
  else root.CHAPTER_SCHEMA = factory();
})(typeof self !== 'undefined' ? self : this, function () {
  const CATS = ['concept', 'field', 'action'];
  const MIN_QUESTIONS = 25;
  const OPTIONS = 5;

  const isStr = v => typeof v === 'string' && v.trim().length > 0;

  function checkItem(item, where, problems) {
    if (isStr(item)) return;
    if (!item || typeof item !== 'object') { problems.push(where + ': an item is neither a string nor an object'); return; }
    const keys = Object.keys(item).filter(k => k !== 'kind' && k !== 'title');
    if (keys.length !== 1) { problems.push(where + ': an item must have exactly one of list, steps, code, table, callout, or pitfalls'); return; }
    const k = keys[0];
    const v = item[k];
    if (k === 'list' || k === 'steps') {
      if (!Array.isArray(v) || v.length === 0 || !v.every(isStr)) problems.push(where + ': ' + k + ' must be a non-empty array of strings');
    } else if (k === 'code' || k === 'callout') {
      if (!isStr(v)) problems.push(where + ': ' + k + ' must be a string');
    } else if (k === 'table') {
      if (!v || !Array.isArray(v.head) || !Array.isArray(v.rows) || !v.rows.every(r => Array.isArray(r) && r.length === v.head.length)) problems.push(where + ': table needs head and rows of the same width');
    } else if (k === 'pitfalls') {
      if (!Array.isArray(v) || v.length === 0 || !v.every(p => p && isStr(p.symptom) && isStr(p.cause) && isStr(p.fix))) problems.push(where + ': every pitfall needs symptom, cause, and fix');
    } else {
      problems.push(where + ': unknown item ' + k);
    }
  }

  function checkExample(ex, where, id, problems) {
    if (!ex || !isStr(ex.title) || !isStr(ex.file)) { problems.push(where + ': an example needs title and file'); return; }
    if (!ex.file.startsWith('examples/' + id + '/')) problems.push(where + ': file ' + ex.file + ' is not under examples/' + id + '/');
    if (ex.notes !== undefined && (!Array.isArray(ex.notes) || !ex.notes.every(n => n && isStr(n.at) && isStr(n.text)))) problems.push(where + ': every note needs at and text');
  }

  function checkQuestion(q, where, problems) {
    if (!q || typeof q !== 'object') { problems.push(where + ': not an object'); return; }
    if (!isStr(q.topic)) problems.push(where + ': topic missing');
    if (!CATS.includes(q.cat)) problems.push(where + ': cat must be one of ' + CATS.join(', '));
    if (!isStr(q.text)) problems.push(where + ': text missing');
    if (!Array.isArray(q.options) || q.options.length !== OPTIONS || !q.options.every(isStr)) problems.push(where + ': exactly ' + OPTIONS + ' option strings are needed');
    else if (new Set(q.options.map(o => o.trim())).size !== OPTIONS) problems.push(where + ': two options read the same');
    if (!Array.isArray(q.correct) || !q.correct.every(i => Number.isInteger(i) && i >= 0 && i < OPTIONS)) problems.push(where + ': correct must list option indices 0 to ' + (OPTIONS - 1));
    else if (new Set(q.correct).size !== q.correct.length) problems.push(where + ': correct lists an index twice');
    if (!isStr(q.explain)) problems.push(where + ': explain missing');
  }

  function placeholders(yaml) {
    const out = [];
    const re = /\{\{([a-zA-Z0-9_]+)\}\}/g;
    let m;
    while ((m = re.exec(yaml)) !== null) out.push(m[1]);
    return out;
  }

  function checkTask(t, where, problems) {
    if (!t || typeof t !== 'object') { problems.push(where + ': not an object'); return; }
    if (!isStr(t.title)) problems.push(where + ': title missing');
    if (!isStr(t.scenario)) problems.push(where + ': scenario missing');
    if (!isStr(t.explain)) problems.push(where + ': explain missing');
    if (!isStr(t.yaml)) { problems.push(where + ': yaml missing'); return; }
    const names = placeholders(t.yaml);
    if (names.length === 0) problems.push(where + ': the yaml has no {{blank}}');
    if (new Set(names).size !== names.length) problems.push(where + ': a blank appears twice in the yaml');
    const blanks = t.blanks && typeof t.blanks === 'object' ? t.blanks : {};
    for (const n of names) {
      const b = blanks[n];
      if (!b || !Array.isArray(b.accept) || b.accept.length === 0 || !b.accept.every(isStr)) problems.push(where + ': blank ' + n + ' needs a non-empty accept list');
      else for (const a of b.accept) {
        if (a.length > 2 && a.startsWith('/') && a.endsWith('/')) {
          try { new RegExp(a.slice(1, -1)); } catch (e) { problems.push(where + ': blank ' + n + ' has an invalid regex ' + a); }
        }
      }
    }
    for (const n of Object.keys(blanks)) if (!names.includes(n)) problems.push(where + ': blank ' + n + ' is defined but not in the yaml');
  }

  // check returns every problem with one chapter object, or an empty list.
  function check(ch) {
    const problems = [];
    if (!ch || typeof ch !== 'object') return ['the chapter is not an object'];
    const id = ch.id;
    if (!isStr(id) || !/^[a-z0-9]+(-[a-z0-9]+)*$/.test(id)) problems.push('id must be kebab-case');
    const where = 'chapter ' + (id || '?');
    for (const f of ['title', 'summary', 'goal']) if (!isStr(ch[f])) problems.push(where + ': ' + f + ' missing');
    if (!Array.isArray(ch.sections) || ch.sections.length < 3) problems.push(where + ': at least three sections are needed');
    else ch.sections.forEach((s, i) => {
      const w = where + ', section ' + (i + 1);
      if (!s || !isStr(s.heading)) problems.push(w + ': heading missing');
      if (!Array.isArray(s.body) || s.body.length === 0) problems.push(w + ': body missing');
      else s.body.forEach((item, j) => checkItem(item, w + ', item ' + (j + 1), problems));
      if (s.examples !== undefined) {
        if (!Array.isArray(s.examples)) problems.push(w + ': examples must be an array');
        else s.examples.forEach((ex, j) => checkExample(ex, w + ', example ' + (j + 1), id, problems));
      }
    });
    if (!Array.isArray(ch.questions) || ch.questions.length < MIN_QUESTIONS) problems.push(where + ': at least ' + MIN_QUESTIONS + ' questions are needed');
    else {
      ch.questions.forEach((q, i) => checkQuestion(q, where + ', question ' + (i + 1), problems));
      const topics = new Set(ch.questions.map(q => q.topic));
      if (topics.size < 3) problems.push(where + ': questions span fewer than three topics');
      const counts = {};
      for (const q of ch.questions) if (Array.isArray(q.correct)) counts[q.correct.length] = (counts[q.correct.length] || 0) + 1;
      const n = ch.questions.length;
      for (const c of Object.keys(counts)) if (counts[c] > n * 0.5) problems.push(where + ': ' + counts[c] + ' of ' + n + ' questions have exactly ' + c + ' correct answers, which makes the count guessable');
    }
    if (!Array.isArray(ch.tasks) || ch.tasks.length < 2 || ch.tasks.length > 4) problems.push(where + ': two to four tasks are needed');
    else ch.tasks.forEach((t, i) => checkTask(t, where + ', task ' + (i + 1), problems));
    return problems;
  }

  // exampleFiles lists every example file a chapter references, for the checker.
  function exampleFiles(ch) {
    const out = [];
    for (const s of ch.sections || []) for (const ex of s.examples || []) if (ex && ex.file) out.push(ex.file);
    return out;
  }

  return { check, exampleFiles, placeholders, MIN_QUESTIONS, OPTIONS, CATS };
});
