<!--
How the operator training is put together and how a chapter is added, for
whoever writes the next one. The pages themselves explain nothing of this.
-->

# Operator training

Training material on the operator, organized by use case: one chapter per
deployment or operation, each with the objects a user writes in the order they
are written, annotated example manifests, and a test. The CRD quiz is the
prerequisite that covers the vocabulary.

| File                   | What it is                                                                 |
|------------------------|----------------------------------------------------------------------------|
| `index.html`           | The front page: the quiz as prerequisite, the chapter list                 |
| `chapter.html?c=<id>`  | One chapter: lesson, examples, test                                        |
| `training.js`          | The engine both pages run                                                  |
| `training.css`         | Their stylesheet                                                           |
| `chapter-schema.js`    | What a chapter file has to contain; run by the page and by the checker     |
| `chapters/index.js`    | The ordered list of chapter files                                          |
| `chapters/NN-<id>.js`  | One chapter each                                                           |
| `examples/<id>/*.yaml` | The example manifests a chapter shows                                      |
| `check-chapters.js`    | Checks every chapter file under node                                       |
| `admin.html`           | The overview of every learner's progress, behind the admin token           |
| `crd-quiz.html`        | The prerequisite quiz, a single page with its question bank inline         |
| `results-api.py`       | The learners' progress, time, and test attempts, and the quiz's statistics |
| `training-host.sh`     | EC2 user data setting up a public copy with nginx and the API              |

## Reading it locally

The pages fetch the example manifests, which a browser refuses from a `file://`
page, so serve the directory:

```bash
cd operator/docs/training && python3 -m http.server 8000
```

Then open http://localhost:8000/. Without `results-api.py` behind `/api/`, the
chapters can be read and the tests taken, and nothing is recorded.

## Learners, progress, and the overview

A learner starts the curriculum by typing a name on the index page. There is no
password: the training is internal, and the same name typed on another device
continues the same record. From then on the API keeps, under that name, which
chapters were opened, the time spent on each, and every finished test. A
chapter is `open` until it is opened, `in-progress` from then on, and
`completed` once a test of it is passed. The time is counted while the chapter
page is visible and the reader was active within the last two minutes, in ticks
of five seconds sent every half minute, so a tab left open in the background
counts nothing.

The chapter statistics the pages show carry no name. The names are read in two
places only: by their owner, and by `admin.html`, which lists every learner with
the status, the time, the attempts, and the best test per chapter, and exports
the table as CSV. It asks for the admin token the host was launched with and
keeps it for the browser session. Without a token in the API's environment the
endpoint answers 503.

## Adding a chapter

1. Write `chapters/NN-<id>.js`, registering one chapter with
   `TRAINING.register({...})`. The opening comment of `chapter-schema.js` is
   the contract: title, summary, goal, at least three sections, at least 25
   questions across at least three topics, and two to four manifest tasks.
   Chapter `01-openshift-compact.js` is the model to copy.
2. Put its example manifests under `examples/<id>/`, numbered in the order the
   lesson shows them. Every manifest of a `storage.simplyblock.io` kind is
   validated against the operator's CRDs, schema and CEL rules alike, by
   `TestTrainingExamplesAreAdmitted` in `support-tool/internal/admission`, so
   a field the API would refuse fails the test before it misleads a reader.
3. Add the file to `chapters/index.js` at its position.
4. Run the checks:

   ```bash
   (cd operator/docs/training && node check-chapters.js)
   (cd support-tool && go test ./internal/admission/ -run TestTrainingExamplesAreAdmitted)
   .claude/skills/house-style/scripts/quality-gate.sh --changed
   ```

What a chapter teaches is what the operator does today. The surveys behind the
first ten chapters found the sources that no longer match the code, and a
chapter reads the code rather than those: the kind table in `operator/README.md`,
the `Ready` phase in `helm-charts/README.md`, and the `v1alpha1` version in the
cluster-config asset of the CRD redesign.

## Questions and scoring

A test draws fifteen questions from the chapter's bank, round-robin over the
topics so no topic dominates, and scores them as the quiz does: every one of the
five answers is judged, a right judgment is +1 and a wrong one is −1, so a
question scores −5 to +5 and the questions −75 to +75. The manifest tasks are
skeletons with blanks; a blank is right when its text equals an accepted value
or matches an accepted `/regex/` in full. A chapter is passed with at least 45
question points and every manifest right.

Write a bank in which the number of correct answers varies, from none to all
five. The checker refuses a bank in which more than half the questions share one
count, because that count becomes guessable.

## Hosting a public copy

`training-host.sh` is EC2 user data for Amazon Linux 2023. Its usage note says
how to pack the directory and substitute the archive's URL and the admin token.
The results store on the host is one JSON file under `/var/lib/crd-quiz`, and
nginx keeps no access log for `/api/`. The host serves plain HTTP, so the
learners' names and the admin token travel in the clear on the network between
the browser and the host.
