// The engine of the operator training program: loads the chapter files, draws
// the index and the chapter pages, runs each chapter's test, and talks to the
// results API.
//
// It is one script for both pages because the index needs what the chapters
// declare (titles, summaries) and the chapter page needs the index's order for
// its previous and next links. What differs between the two pages is one call
// at the bottom, chosen by the body's data-page attribute.
//
// The test is the quiz's test with a second half. Fifteen questions are drawn
// from the chapter's bank and scored as crd-quiz.html scores them: every answer
// is judged, a right judgment is +1 and a wrong one is −1. Then two to four
// manifest tasks follow, a YAML skeleton with blanks to fill in, each blank
// judged against the values a correct manifest carries. A chapter is passed with
// at least PASS_POINTS of the 75 question points and every task right.
//
// The pages have to be served over HTTP: the example manifests are fetched, and
// a browser refuses fetch from a file:// page. `python3 -m http.server` in this
// directory is enough; training-host.sh sets up the public copy.
(function () {
  const PER_RUN = 15;
  const PER_QUESTION = 5;
  const MAX_POINTS = PER_RUN * PER_QUESTION;
  const PASS_POINTS = 45;
  const API = 'api/results';
  const QUIZ_PAGE = 'crd-quiz.html';

  const chapters = [];
  window.TRAINING = { register: ch => chapters.push(ch) };

  // ---- text helpers --------------------------------------------------------
  const esc = s => String(s).replace(/[&<>"]/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' }[c]));
  // Inline markup in prose: `code`, **bold**, and [text](url) for relative or https links.
  const fmt = s => esc(s)
    .replace(/`([^`]+)`/g, '<code>$1</code>')
    .replace(/\*\*([^*]+)\*\*/g, '<strong>$1</strong>')
    .replace(/\[([^\]]+)\]\(((?:https:\/\/|[a-z0-9_./-])[^)\s]*)\)/g, '<a href="$2">$1</a>');
  const shuffle = a => { a = a.slice(); for (let i = a.length - 1; i > 0; i--) { const j = Math.floor(Math.random() * (i + 1)); [a[i], a[j]] = [a[j], a[i]]; } return a; };
  const signed = n => (n > 0 ? '+' : '') + n;
  const catClass = c => c === 'action' ? 'chip action' : c === 'concept' ? 'chip concept' : 'chip';
  const catHint = c => c === 'field' ? 'Field meaning' : c === 'action' ? 'Operation' : 'Concept';
  const slug = s => String(s).toLowerCase().replace(/[^a-z0-9]+/g, '-').replace(/^-|-$/g, '');
  const plural = (n, one, many) => n === 1 ? one : many;

  // ---- scoring -------------------------------------------------------------
  const judge = (options, picked) => {
    const hits = options.filter((o, idx) => picked.includes(idx) === o.correct).length;
    return { hits, misses: options.length - hits, points: hits - (options.length - hits) };
  };
  const totalPoints = results => results.reduce((s, r) => s + judge(r.options, r.picked).points, 0);

  // A blank is right when its trimmed text equals an accepted value, or matches
  // an accepted /regex/ in full.
  function blankRight(blank, value) {
    const v = String(value || '').trim();
    return blank.accept.some(a => {
      if (a.length > 2 && a.startsWith('/') && a.endsWith('/')) return new RegExp('^(?:' + a.slice(1, -1) + ')$').test(v);
      return a === v;
    });
  }
  const taskRight = (task, values) => Object.keys(task.blanks).every(n => blankRight(task.blanks[n], values[n]));

  // ---- storage and API -----------------------------------------------------
  function bestGet(id) { try { return JSON.parse(localStorage.getItem('training.' + id + '.best') || 'null'); } catch (e) { return null; } }
  function bestSet(id, v) { try { localStorage.setItem('training.' + id + '.best', JSON.stringify(v)); } catch (e) { /* storage may be unavailable */ } }

  let stats = null;        // the last answer of GET api/results, or null
  let apiAvailable = false;

  async function fetchStats() {
    try {
      const r = await fetch(API, { cache: 'no-store' });
      if (!r.ok) throw new Error(String(r.status));
      stats = await r.json();
      apiAvailable = stats && typeof stats.chapters === 'object';
    } catch (e) {
      apiAvailable = false;
    }
    return apiAvailable;
  }

  async function sendResult(id, points, passed) {
    const r = await fetch(API + '/' + encodeURIComponent(id), {
      method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ points, passed }),
    });
    if (!r.ok) throw new Error('The server answered ' + r.status + '.');
    const summary = await r.json();
    if (stats && stats.chapters) stats.chapters[id] = summary;
  }

  function chapterStats(id) {
    const s = stats && stats.chapters && stats.chapters[id];
    return s && Array.isArray(s.scores) ? s : null;
  }

  function statsBlock(id, mine) {
    const s = chapterStats(id);
    if (!s) return '<p class="note">Statistics could not be loaded.</p>';
    const n = s.scores.length;
    if (n === 0) return '<p class="note">No result has been sent in for this chapter yet.</p>';
    const avg = s.scores.reduce((a, b) => a + b.points, 0) / n;
    const passed = s.scores.filter(x => x.passed).length;
    const chips = s.scores.map(x => `<span class="${x.passed ? 'p' : ''}">${signed(x.points)}</span>`);
    if (mine) {
      // The sender's own result is highlighted once, on the last matching entry.
      for (let i = chips.length - 1; i >= 0; i--) {
        if (s.scores[i].points === mine.points && s.scores[i].passed === mine.passed) { chips[i] = `<span class="mine${mine.passed ? ' p' : ''}">${signed(mine.points)}</span>`; break; }
      }
    }
    return `
      <div class="score">
        <div><div class="n">${signed(Math.round(avg * 10) / 10)}</div><div class="l">average question points over ${n} ${plural(n, 'test', 'tests')} sent in (out of ${MAX_POINTS})</div></div>
        <div><div class="n">${Math.round((passed / n) * 100)}%</div><div class="l">passed: ${passed} of ${n}</div></div>
      </div>
      <div class="stack tight">
        <p class="eyebrow">Every test sent in</p>
        <div class="scores">${chips.join('')}</div>
        <p class="note">Each entry is one finished test whose taker chose to send it; a check mark means it was passed. No name, address, or time is stored with it.</p>
      </div>`;
  }

  // ---- loading the chapter files ------------------------------------------
  function loadScript(src) {
    return new Promise((resolve, reject) => {
      const s = document.createElement('script');
      s.src = src;
      s.onload = resolve;
      s.onerror = () => reject(new Error(src + ' did not load'));
      document.head.appendChild(s);
    });
  }

  async function loadChapters() {
    const problems = [];
    for (const f of CHAPTER_FILES) {
      try { await loadScript('chapters/' + f); } catch (e) { problems.push(e.message); }
    }
    chapters.forEach((ch, i) => { ch.number = i + 1; });
    if (window.CHAPTER_SCHEMA) for (const ch of chapters) problems.push(...CHAPTER_SCHEMA.check(ch));
    const ids = chapters.map(c => c.id);
    if (new Set(ids).size !== ids.length) problems.push('two chapters share an id');
    return problems;
  }

  const problemsBlock = problems => problems.length === 0 ? '' :
    `<div class="problems"><strong>This copy has problems a reader should not see:</strong><ul>${problems.map(p => `<li>${esc(p)}</li>`).join('')}</ul></div>`;

  // ---- the index page -------------------------------------------------------
  function renderIndex(problems) {
    const app = document.getElementById('app');
    const cards = chapters.map(ch => {
      const best = bestGet(ch.id);
      const s = chapterStats(ch.id);
      let st = '';
      if (best && best.passed) st = '<span class="st pass">Passed on this device</span>';
      else if (best) st = `<span class="st muted">Best on this device: ${signed(best.points)} points, not yet passed</span>`;
      else if (s && s.scores.length) st = `<span class="st muted">${s.scores.length} ${plural(s.scores.length, 'test', 'tests')} sent in, ${Math.round(100 * s.scores.filter(x => x.passed).length / s.scores.length)}% passed</span>`;
      return `<a class="chapter" href="chapter.html?c=${encodeURIComponent(ch.id)}"><span class="num">Chapter ${ch.number}</span><span class="t">${esc(ch.title)}</span><span class="s">${fmt(ch.summary)}</span>${st}</a>`;
    });
    app.innerHTML = `
      ${problemsBlock(problems)}
      <div class="stack">
        <p class="eyebrow">simplyblock operator · storage.simplyblock.io</p>
        <h1>Operator training by use case</h1>
        <p class="muted">Each chapter takes one deployment or operation the way it is done in practice: the objects a user writes, in the order they are written, what the operator does with each, and what goes wrong. It ends in a test of fifteen questions and a few manifests to complete. Passing a chapter takes ${PASS_POINTS} of ${MAX_POINTS} question points and every manifest right.</p>
      </div>
      <div class="card stack">
        <h2>Before the chapters</h2>
        <p>The chapters assume the vocabulary of the custom resources: which kinds are entities and which are operations, what <code>status.activeOpsRef</code> locks, and how the ownership spine runs from the ControlPlane down to the devices. The CRD quiz is the fastest way to find out whether that is in place.</p>
        <div class="row"><a class="chapter prereq" href="${QUIZ_PAGE}" style="flex:1"><span class="num">Prerequisite</span><span class="t">Operator CRD Quiz</span><span class="s">Fifteen random questions on what each kind and field is for.</span></a></div>
      </div>
      <div class="stack">
        <h2>Chapters</h2>
        <div class="chapters">${cards.join('')}</div>
        <p class="note">${apiAvailable ? 'Results sent in from any copy of a chapter are counted in that chapter\'s statistics, anonymously.' : 'This copy is not connected to a results server, so tests can be taken but not sent in.'}</p>
      </div>`;
  }

  // ---- the chapter page: lesson --------------------------------------------
  function renderItem(item) {
    if (typeof item === 'string') return `<p>${fmt(item)}</p>`;
    if (item.list) return `<ul>${item.list.map(x => `<li>${fmt(x)}</li>`).join('')}</ul>`;
    if (item.steps) return `<ol>${item.steps.map(x => `<li>${fmt(x)}</li>`).join('')}</ol>`;
    if (item.code) return `${item.title ? `<p class="eyebrow">${esc(item.title)}</p>` : ''}<pre><code>${esc(item.code)}</code></pre>`;
    if (item.table) return `<table><thead><tr>${item.table.head.map(h => `<th>${fmt(h)}</th>`).join('')}</tr></thead><tbody>${item.table.rows.map(r => `<tr>${r.map(c => `<td>${fmt(c)}</td>`).join('')}</tr>`).join('')}</tbody></table>`;
    if (item.callout) return `<div class="callout${item.kind === 'warn' ? ' warn' : ''}">${fmt(item.callout)}</div>`;
    if (item.pitfalls) return item.pitfalls.map(p => `<div class="pitfall"><b>Symptom</b><span>${fmt(p.symptom)}</span><b>Cause</b><span>${fmt(p.cause)}</span><b>Fix</b><span>${fmt(p.fix)}</span></div>`).join('');
    return '';
  }

  // An example is drawn once its file arrives: every line, with the noted ones
  // marked and numbered, and the notes beneath in the same order.
  function renderExample(ex, idx) {
    return `<div class="example" id="ex${idx}"><div class="head"><strong>${fmt(ex.title)}</strong><a class="f" href="${esc(ex.file)}">${esc(ex.file.replace(/^examples\/[^/]+\//, ''))}</a></div><div class="body"><p class="note" style="padding:12px 14px">Loading…</p></div></div>`;
  }

  async function fillExample(ex, idx) {
    const el = document.querySelector(`#ex${idx} .body`);
    if (!el) return;
    let text;
    try {
      const r = await fetch(ex.file, { cache: 'no-store' });
      if (!r.ok) throw new Error('the server answered ' + r.status);
      text = await r.text();
    } catch (e) {
      el.innerHTML = `<p class="missing">The manifest could not be loaded (${esc(e.message)}). This page has to be served over HTTP, for example with <code>python3 -m http.server</code> in the training directory.</p>`;
      return;
    }
    const lines = text.replace(/\s+$/, '').split('\n');
    const marks = new Array(lines.length).fill(0);
    const notes = ex.notes || [];
    notes.forEach((n, i) => {
      const at = lines.findIndex((l, li) => marks[li] === 0 && l.includes(n.at));
      if (at >= 0) marks[at] = i + 1;
    });
    // Each line is its own block, so no newline is written between them.
    el.innerHTML = `<pre>${lines.map((l, i) => `<span class="ln${marks[i] ? ' marked' : ''}">${esc(l)}${marks[i] ? `<span class="m">${marks[i]}</span>` : ''}</span>`).join('')}</pre>` +
      (notes.length ? `<ul class="notes">${notes.map((n, i) => `<li><span class="m">${i + 1}</span><span>${fmt(n.text)}</span></li>`).join('')}</ul>` : '');
  }

  function renderLesson(ch, problems) {
    const app = document.getElementById('app');
    const prev = chapters[ch.number - 2];
    const next = chapters[ch.number];
    const best = bestGet(ch.id);
    let exIdx = 0;
    const examples = [];
    const sections = ch.sections.map((s, i) => {
      const id = 's' + (i + 1) + '-' + slug(s.heading);
      const exs = (s.examples || []).map(ex => { const html = renderExample(ex, exIdx); examples.push([ex, exIdx]); exIdx++; return html; });
      return `<section class="section card" id="${id}"><h2>${fmt(s.heading)}</h2>${s.body.map(renderItem).join('')}${exs.join('')}</section>`;
    });
    app.innerHTML = `
      ${problemsBlock(problems)}
      <div class="stack">
        <p class="crumbs"><a href="index.html">Operator training</a> · Chapter ${ch.number} of ${chapters.length}</p>
        <h1>${esc(ch.title)}</h1>
        <p class="muted">${fmt(ch.summary)}</p>
        <div class="callout"><strong>After this chapter</strong> ${fmt(ch.goal)}</div>
        <nav class="toc">${ch.sections.map((s, i) => `<a href="#s${i + 1}-${slug(s.heading)}">${i + 1}. ${esc(s.heading)}</a>`).join('')}<a href="#test">Test</a></nav>
      </div>
      ${sections.join('')}
      <section class="section card" id="test">
        <h2>Test</h2>
        <p>Fifteen questions drawn from a bank of ${ch.questions.length}, with five answers each of which any number can be correct, then ${ch.tasks.length} ${plural(ch.tasks.length, 'manifest', 'manifests')} to complete. Every answer is judged, +1 for a right judgment and −1 for a wrong one, so the questions score −${MAX_POINTS} to +${MAX_POINTS}. The chapter is passed with at least ${PASS_POINTS} points and every manifest right.</p>
        <div class="row">
          <button class="primary" id="startBtn">Start the test</button>
          ${apiAvailable ? '<button id="statsBtn">Statistics</button>' : ''}
          ${best ? `<span class="small muted">${best.passed ? 'Passed on this device' : 'Best on this device: ' + signed(best.points) + ' points, not yet passed'}</span>` : ''}
        </div>
      </section>
      <div class="row">
        ${prev ? `<a href="chapter.html?c=${encodeURIComponent(prev.id)}">← ${esc(prev.title)}</a>` : ''}
        ${next ? `<a href="chapter.html?c=${encodeURIComponent(next.id)}" style="margin-left:auto">${esc(next.title)} →</a>` : ''}
      </div>`;
    document.getElementById('startBtn').addEventListener('click', () => startTest(ch));
    const sb = document.getElementById('statsBtn');
    if (sb) sb.addEventListener('click', async () => { await fetchStats(); state.screen = 'stats'; render(); window.scrollTo({ top: 0 }); });
    examples.forEach(([ex, i]) => fillExample(ex, i));
  }

  // ---- the chapter page: test ---------------------------------------------
  let state = { screen: 'lesson' };
  let current = null;      // the chapter being shown
  let loadProblems = [];

  function pickRun(ch) {
    // Round-robin over shuffled topics, so no single topic dominates a run.
    const byTopic = {};
    shuffle(ch.questions).forEach(q => { (byTopic[q.topic] = byTopic[q.topic] || []).push(q); });
    const topics = shuffle(Object.keys(byTopic));
    const out = [];
    let round = 0;
    while (out.length < PER_RUN && round < 50) {
      for (const t of topics) {
        if (out.length >= PER_RUN) break;
        const q = byTopic[t][round];
        if (q) out.push(q);
      }
      round++;
    }
    return shuffle(out).map(q => ({ q, options: shuffle(q.options.map((text, idx) => ({ text, correct: q.correct.includes(idx) }))) }));
  }

  function startTest(ch) {
    state = { screen: 'question', run: pickRun(ch), i: 0, picked: [], revealed: false, results: [], t: 0, taskValues: {}, taskChecked: false, taskResults: [], sent: false };
    render();
    window.scrollTo({ top: 0 });
  }

  function renderQuestion(ch) {
    const app = document.getElementById('app');
    const item = state.run[state.i];
    const q = item.q;
    const n = state.i + 1;
    const total = PER_RUN + ch.tasks.length;
    const pct = Math.round((state.i / total) * 100);
    const picked = state.picked;
    const opts = item.options.map((o, idx) => {
      const on = picked.includes(idx);
      let cls = 'opt' + (on ? ' checked' : '');
      let mark = '';
      if (state.revealed) {
        const judged = on === o.correct;
        cls += judged ? ' right' : ' wrong';
        mark = `<span class="mark">${o.correct ? 'correct' : 'not correct'}${judged ? ' · +1' : ' · you missed this · −1'}</span>`;
      }
      return `<li><label class="${cls}"><input type="checkbox" data-idx="${idx}" ${on ? 'checked' : ''} ${state.revealed ? 'disabled' : ''}><span>${fmt(o.text)}</span>${mark}</label></li>`;
    }).join('');
    let verdict = '';
    if (state.revealed) {
      const j = judge(item.options, picked);
      const fullyRight = j.misses === 0;
      verdict = `<div class="verdict ${fullyRight ? 'ok' : 'miss'}"><strong>${fullyRight ? 'Correct: +5 points.' : `${j.hits} right, ${j.misses} wrong: ${signed(j.points)} points.`}</strong> ${fmt(q.explain)}</div>`;
    }
    app.innerHTML = `
      <p class="crumbs"><a href="index.html">Operator training</a> · Chapter ${ch.number}: ${esc(ch.title)}</p>
      <div class="progress"><div class="bar"><i style="width:${pct}%"></i></div><span class="counter">Question ${n} of ${PER_RUN}</span></div>
      <div class="card stack">
        <div class="row"><span class="${catClass(q.cat)}">${esc(q.topic)}</span><span class="hint">${catHint(q.cat)}</span></div>
        <p class="qtext">${fmt(q.text)}</p>
        <ul class="opts" id="opts">${opts}</ul>
        ${verdict}
        <div class="row">
          ${state.revealed
            ? `<button class="primary" id="nextBtn">${n === PER_RUN ? 'On to the manifests' : 'Next question'}</button>`
            : `<button class="primary" id="checkBtn">Check answer</button><span class="hint">${picked.length === 0 ? 'Nothing ticked means "none of these is correct".' : `${picked.length} ticked`}</span>`}
        </div>
      </div>`;
    if (!state.revealed) {
      document.getElementById('opts').addEventListener('change', e => {
        const idx = Number(e.target.dataset.idx);
        state.picked = e.target.checked ? [...state.picked, idx] : state.picked.filter(x => x !== idx);
        renderQuestion(ch);
      });
      document.getElementById('checkBtn').addEventListener('click', () => {
        state.revealed = true;
        const fullyRight = item.options.every((o, idx) => state.picked.includes(idx) === o.correct);
        state.results.push({ q, options: item.options, picked: state.picked, fullyRight });
        renderQuestion(ch);
      });
    } else {
      document.getElementById('nextBtn').addEventListener('click', () => {
        if (state.i + 1 >= PER_RUN) state.screen = 'task';
        else { state.i++; state.picked = []; state.revealed = false; }
        render();
        window.scrollTo({ top: 0 });
      });
    }
  }

  // The skeleton is drawn as a <pre> in which every {{blank}} is an input. The
  // input's width follows what is typed, so the YAML keeps its shape.
  function renderSkeleton(task, values, checked) {
    const parts = task.yaml.split(/(\{\{[a-zA-Z0-9_]+\}\})/);
    return parts.map(p => {
      const m = /^\{\{([a-zA-Z0-9_]+)\}\}$/.exec(p);
      if (!m) return esc(p);
      const name = m[1];
      const v = values[name] || '';
      const cls = 'blank' + (checked ? (blankRight(task.blanks[name], v) ? ' right' : ' wrong') : '');
      const size = Math.max(8, v.length + 1);
      return `<input class="${cls}" data-blank="${name}" value="${esc(v)}" size="${size}" spellcheck="false" autocomplete="off" ${checked ? 'disabled' : ''} aria-label="${esc(name)}">`;
    }).join('');
  }

  function renderTask(ch) {
    const app = document.getElementById('app');
    const task = ch.tasks[state.t];
    const n = state.t + 1;
    const total = PER_RUN + ch.tasks.length;
    const pct = Math.round(((PER_RUN + state.t) / total) * 100);
    const values = state.taskValues;
    const checked = state.taskChecked;
    const names = Object.keys(task.blanks);
    let verdict = '';
    if (checked) {
      const right = names.filter(nm => blankRight(task.blanks[nm], values[nm])).length;
      verdict = `
        <div class="verdict ${right === names.length ? 'ok' : 'miss'}"><strong>${right === names.length ? 'Every blank is right.' : `${right} of ${names.length} ${plural(names.length, 'blank', 'blanks')} right.`}</strong> ${fmt(task.explain)}</div>
        <ul class="answers">${names.map(nm => {
          const ok = blankRight(task.blanks[nm], values[nm]);
          return `<li><span class="${ok ? 'ok' : 'miss'}">${ok ? '✓' : '✗'}</span><code>${esc(nm)}</code><span>${ok ? `<code>${esc(values[nm] || '')}</code>` : `you wrote <code>${esc(values[nm] || '(nothing)')}</code>, expected <code>${esc(task.blanks[nm].accept[0].replace(/^\/|\/$/g, ''))}</code>`}${task.blanks[nm].hint ? ` · ${fmt(task.blanks[nm].hint)}` : ''}</span></li>`; }).join('')}</ul>`;
    }
    app.innerHTML = `
      <p class="crumbs"><a href="index.html">Operator training</a> · Chapter ${ch.number}: ${esc(ch.title)}</p>
      <div class="progress"><div class="bar"><i style="width:${pct}%"></i></div><span class="counter">Manifest ${n} of ${ch.tasks.length}</span></div>
      <div class="card stack task">
        <div class="row"><span class="chip action">manifest</span><span class="hint">Fill every blank so the manifest does what the scenario asks</span></div>
        <h2>${fmt(task.title)}</h2>
        <p>${fmt(task.scenario)}</p>
        <pre id="skeleton">${renderSkeleton(task, values, checked)}</pre>
        ${verdict}
        <div class="row">
          ${checked
            ? `<button class="primary" id="nextBtn">${n === ch.tasks.length ? 'See results' : 'Next manifest'}</button>`
            : `<button class="primary" id="checkBtn">Check manifest</button><span class="hint">${names.filter(nm => (values[nm] || '').trim()).length} of ${names.length} ${plural(names.length, 'blank', 'blanks')} filled</span>`}
        </div>
      </div>`;
    if (!checked) {
      const sk = document.getElementById('skeleton');
      sk.addEventListener('input', e => {
        const nm = e.target.dataset.blank;
        if (!nm) return;
        state.taskValues[nm] = e.target.value;
        e.target.size = Math.max(8, e.target.value.length + 1);
        const hint = app.querySelector('.row .hint:last-child');
        if (hint) hint.textContent = `${names.filter(x => (state.taskValues[x] || '').trim()).length} of ${names.length} ${plural(names.length, 'blank', 'blanks')} filled`;
      });
      document.getElementById('checkBtn').addEventListener('click', () => {
        state.taskChecked = true;
        state.taskResults.push({ task, values: Object.assign({}, state.taskValues), right: taskRight(task, state.taskValues) });
        renderTask(ch);
      });
    } else {
      document.getElementById('nextBtn').addEventListener('click', () => {
        if (state.t + 1 >= ch.tasks.length) state.screen = 'results';
        else { state.t++; state.taskValues = {}; state.taskChecked = false; }
        render();
        window.scrollTo({ top: 0 });
      });
    }
  }

  function renderResults(ch, sendError) {
    const app = document.getElementById('app');
    const points = totalPoints(state.results);
    const fully = state.results.filter(r => r.fullyRight).length;
    const tasksRight = state.taskResults.filter(r => r.right).length;
    const passed = points >= PASS_POINTS && tasksRight === ch.tasks.length;
    const prevBest = bestGet(ch.id);
    if (!prevBest || passed && !prevBest.passed || passed === !!prevBest.passed && points > prevBest.points) bestSet(ch.id, { points, passed });
    const why = passed ? 'Passed.' : points < PASS_POINTS && tasksRight < ch.tasks.length ? `Not passed: ${PASS_POINTS} points and every manifest right are needed.` : points < PASS_POINTS ? `Not passed: the manifests are right, but ${PASS_POINTS} question points are needed.` : 'Not passed: the questions are fine, but every manifest has to be right.';
    let share;
    if (!apiAvailable) share = '<p class="note">This copy is not connected to a results server, so results cannot be sent in.</p>';
    else if (state.sent) share = `<div class="stack"><p><strong>Sent.</strong> Your result is counted in the statistics of this chapter.</p>${statsBlock(ch.id, { points, passed })}</div>`;
    else share = `
        <div class="stack tight">
          <p>Count this test in the chapter's shared statistics? Only the question points (${signed(points)}) and whether the chapter was passed are sent. No name, address, or time goes with it, and everyone can see the statistics.</p>
          <div class="row"><button class="primary" id="sendBtn">Send my result anonymously</button><button id="statsBtn">Show statistics without sending</button></div>
          ${sendError ? `<p class="note" style="color:var(--bad)">Sending failed: ${esc(sendError)} You can try again.</p>` : ''}
        </div>`;
    app.innerHTML = `
      <div class="stack">
        <p class="crumbs"><a href="index.html">Operator training</a> · Chapter ${ch.number}: ${esc(ch.title)}</p>
        <h1>${signed(points)} points, ${tasksRight} of ${ch.tasks.length} ${plural(ch.tasks.length, 'manifest', 'manifests')} right</h1>
        <p><span class="${passed ? 'pass' : 'fail'}">${esc(why)}</span></p>
      </div>
      <div class="card stack">
        <div class="score">
          <div><div class="n">${signed(points)}</div><div class="l">question points: right judgments minus wrong ones over ${MAX_POINTS} answers</div></div>
          <div><div class="n">${fully}<span class="muted" style="font-size:1.2rem">/${PER_RUN}</span></div><div class="l">questions with all five answers judged right</div></div>
          <div><div class="n">${tasksRight}<span class="muted" style="font-size:1.2rem">/${ch.tasks.length}</span></div><div class="l">manifests with every blank right</div></div>
        </div>
        ${share}
        <div class="row"><button class="primary" id="againBtn">Take the test again</button><button id="lessonBtn">Back to the chapter</button>${chapters[ch.number] ? `<a href="chapter.html?c=${encodeURIComponent(chapters[ch.number].id)}" style="margin-left:auto">Next chapter →</a>` : ''}</div>
      </div>
      <div class="stack">
        <h2>Review</h2>
        <div class="review">${state.results.map(r => {
          const j = judge(r.options, r.picked);
          return `<details ${r.fullyRight ? '' : 'open'}>
            <summary><span class="tag ${r.fullyRight ? 'ok' : 'miss'}">${signed(j.points)}</span><span>${fmt(r.q.text)}</span></summary>
            <ul>${r.options.map((o, idx) => `<li class="${o.correct ? 'c' : 'w'}">${fmt(o.text)}${r.picked.includes(idx) !== o.correct ? ' <em>(you got this one wrong)</em>' : ''}</li>`).join('')}</ul>
            <p class="small" style="margin-top:8px">${fmt(r.q.explain)}</p>
          </details>`; }).join('')}
        ${state.taskResults.map(r => `<details ${r.right ? '' : 'open'}>
            <summary><span class="tag ${r.right ? 'ok' : 'miss'}">${r.right ? 'right' : 'wrong'}</span><span>${fmt(r.task.title)}</span></summary>
            <ul>${Object.keys(r.task.blanks).map(nm => `<li class="${blankRight(r.task.blanks[nm], r.values[nm]) ? 'c' : 'w'}"><code>${esc(nm)}</code>: ${blankRight(r.task.blanks[nm], r.values[nm]) ? `<code>${esc(r.values[nm])}</code>` : `you wrote <code>${esc(r.values[nm] || '(nothing)')}</code>, expected <code>${esc(r.task.blanks[nm].accept[0].replace(/^\/|\/$/g, ''))}</code>`}</li>`).join('')}</ul>
            <p class="small" style="margin-top:8px">${fmt(r.task.explain)}</p>
          </details>`).join('')}</div>
      </div>`;
    document.getElementById('againBtn').addEventListener('click', () => startTest(ch));
    document.getElementById('lessonBtn').addEventListener('click', () => { state = { screen: 'lesson' }; render(); window.scrollTo({ top: 0 }); });
    const send = document.getElementById('sendBtn');
    if (send) send.addEventListener('click', async () => {
      send.disabled = true; send.textContent = 'Sending…';
      try { await sendResult(ch.id, points, passed); state.sent = true; renderResults(ch); }
      catch (e) { renderResults(ch, e.message || 'network error.'); }
    });
    const sb = document.getElementById('statsBtn');
    if (sb) sb.addEventListener('click', async () => { await fetchStats(); state.screen = 'stats'; render(); window.scrollTo({ top: 0 }); });
  }

  function renderStats(ch) {
    const app = document.getElementById('app');
    app.innerHTML = `
      <div class="stack">
        <p class="crumbs"><a href="index.html">Operator training</a> · Chapter ${ch.number}: ${esc(ch.title)}</p>
        <h1>Statistics</h1>
        <p class="muted">Every test of this chapter that was taken to the end and sent in, anonymously.</p>
      </div>
      <div class="card stack">
        ${statsBlock(ch.id)}
        <div class="row"><button class="primary" id="startBtn">Start the test</button><button id="lessonBtn">Back to the chapter</button></div>
      </div>`;
    document.getElementById('startBtn').addEventListener('click', () => startTest(ch));
    document.getElementById('lessonBtn').addEventListener('click', () => { state = { screen: 'lesson' }; render(); window.scrollTo({ top: 0 }); });
  }

  function render() {
    if (state.screen === 'lesson') renderLesson(current, loadProblems);
    else if (state.screen === 'question') renderQuestion(current);
    else if (state.screen === 'task') renderTask(current);
    else if (state.screen === 'stats') renderStats(current);
    else renderResults(current);
  }

  // ---- boot ----------------------------------------------------------------
  async function boot() {
    const page = document.body.dataset.page;
    loadProblems = await loadChapters();
    if (page === 'index') {
      renderIndex(loadProblems);
      if (await fetchStats()) renderIndex(loadProblems);
      return;
    }
    const id = new URLSearchParams(location.search).get('c');
    current = chapters.find(c => c.id === id) || chapters[0];
    if (!current) { document.getElementById('app').innerHTML = problemsBlock(loadProblems.concat(['no chapter loaded'])); return; }
    if (id && current.id !== id) loadProblems = loadProblems.concat([`there is no chapter ${id}; showing the first`]);
    document.title = `${current.number}. ${current.title} · Operator training`;
    render();
    // The first render goes out before the API answers; the test section gains
    // its Statistics button once it has.
    if (await fetchStats() && state.screen === 'lesson') render();
  }
  boot();
})();
