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
| `training.css`         | The stylesheet every page shares, after the simplyblock design system      |
| `mark.svg`             | The simplyblock mark in white, for the navy header band                    |
| `chapter-schema.js`    | What a chapter file has to contain; run by the page and by the checker     |
| `chapters/index.js`    | The parts and the ordered list of chapter files                            |
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

## The look

Every page follows the "simplyblock team" design system: Inter at 400, 500, and
600, one meaning per color (blue for simplyblock and links, green for an
outcome such as a passed test, red for a gap and only as text, navy for the
frame), tinted tiles without borders or shadows with white strips inside them,
and the 4px blue-to-green rail across the top. `training.css` holds the tokens
of both themes. The system is written for slides and print, and the stylesheet
departs from it in two places a web page needs: code in a monospace font so
YAML keeps its columns, and a thin outline on form fields.

## Learners, accounts, and the overview

A learner registers on the index page with a name, an email address, and a
password of at least ten characters, or signs in. The session is an HttpOnly
cookie, SameSite=Strict, valid for 30 days, and marked Secure when the host
serves HTTPS. Passwords are stored as salted PBKDF2 hashes and session tokens as
SHA-256 hashes. The host can restrict registration to email domains. There is no
email delivery, so a forgotten password is reset on the overview, which shows a
temporary password once; the learner chooses a new one at the next sign-in.

From then on the API keeps, under the account, which chapters were opened, the
time spent on each, and every finished test. A chapter is `open` until it is
opened, `in-progress` from then on, and `completed` once a test of it is passed.
The time is counted while the chapter page is visible and the reader was active
within the last two minutes, in ticks of five seconds sent every half minute, so
a tab left open in the background counts nothing.

The chapter statistics the pages show carry no name. Names and addresses are
read in two places only: by their owner, and by `admin.html`, which lists every
learner with the status, the time, the attempts, and the best test per chapter,
exports the table as CSV, and resets or deletes an account. It asks for the
admin token the host was launched with and keeps it for the browser session.
Without a token in the API's environment the admin endpoints answer 503.

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

A chapter teaches what the released product does. Its facts come from, in order
of authority:

1. The code on `main`: this operator, the control plane and storage-node agent
   in `simplyblock/sbcli`, and the storage engine on the `new_md_journal` branch
   of simplyblock's SPDK.
2. The public documentation on `main` of `simplyblock/documentation`.

Where the two disagree, the chapter follows the code, and the documentation
gets a fix of its own. A chapter teaches only fields on `main`, never ones that
exist only in open pull requests.

How a chapter is written:

- For a platform engineer who knows Kubernetes and is new to simplyblock,
  addressed as you. Short sentences, everyday words, one idea per sentence.
- Facts about the product, never about where they came from: no talk of the
  code, the design, or this repository.
- Every section that explains something shows it: a command with the output to
  expect, a short YAML snippet, or a worked number.

## Questions and scoring

A test draws fifteen questions from the chapter's bank, round-robin over the
topics so no topic dominates, and scores them as the quiz does: every one of the
five answers is judged, a right judgment is +1 and a wrong one is −1, so a
question scores −5 to +5 and the questions −75 to +75. The manifest tasks are
skeletons with blanks; a blank is right when its text equals an accepted value
or matches an accepted `/regex/` in full. A chapter is passed with at least 50
question points and two thirds of its manifests right, rounded up: 2 of 3, or 3
of 4.

Write a bank in which the number of correct answers varies, from none to all
five. The checker refuses a bank in which more than half the questions share one
count, because that count becomes guessable.

## Hosting a public copy

`training-host.sh` is EC2 user data for Amazon Linux 2023. Its usage note says
how to pack the directory and substitute the archive's URL, the admin token,
the optional registration domains, and the optional host name for HTTPS. The
results store on the host is one JSON file under `/var/lib/crd-quiz`, and nginx
keeps no access log for `/api/`. Signing in and registering are limited to six
requests a minute per address.

Serve HTTPS whenever learners register. With `TLS_DOMAIN` set the host obtains
a Let's Encrypt certificate through the HTTP-01 challenge, which accepts Let's
Encrypt's subscriber agreement, redirects HTTP to HTTPS, and renews the
certificate on a timer; port 443 has to be open beside port 80. A host without
a DNS name of its own can use a name that resolves to its address, such as
`<a-b-c-d>.sslip.io`, and `TLS_DOMAIN=sslip` has the host derive that name from its own public address at boot. Without HTTPS the passwords, the session cookie, and the
admin token cross the network in the clear.
