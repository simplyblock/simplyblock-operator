"""Shared helpers for the house style quality gates.

Ported from the simplyblock documentation repository and adapted for this
repository: the design documents and test plans below operator/docs/, and the
prose that lives in the comments of Go, Python, and YAML sources.

The gates all walk the same set of Markdown files and all have to tell prose
apart from the parts of a page that are not prose: code blocks, inline code,
mkdocs-macros expressions, link targets and raw HTML.

Two levels are offered:

* The primitives (walk, read_lines, is_generated, non_prose_ranges, ...), for a
  gate that runs its own line analysis.
* iter_prose_lines(), for a gate that only wants the prose of a page. It yields
  one entry per line that carries prose, with every non-prose region blanked out,
  so a match position still refers to the original line.
"""

import os
import re
import sys
from dataclasses import dataclass

# The documents the gates own. Source files are scanned when they are passed
# explicitly, or through "quality-gate.sh --changed", so that a run over the
# defaults does not drown in the prose of a repository that predates the gates.
DEFAULT_TARGET_DIRS = ["operator/docs"]

# Markdown carries prose directly. In a source file only the comments do, and
# the extractors further down blank out everything else.
SCANNED_EXTENSIONS = {".md", ".go", ".py", ".yaml", ".yml"}

SYNTAX_OF_EXTENSION = {
    ".md": "markdown",
    ".go": "go",
    ".py": "python",
    ".yaml": "yaml",
    ".yml": "yaml",
}

# Directories that hold no hand-written prose: dependencies, build output,
# vendored charts, caches, and the artifact directories of test runs.
EXCLUDED_DIR_NAMES = {
    ".git", ".idea", ".bin", ".ruff_cache", ".mypy_cache", ".pytest_cache",
    ".tox", ".venv", "venv", "site-packages", "__pycache__", "node_modules",
    "vendor", "bin", "dist", "build", "megalinter-reports",
    "sbcli-repo", "operator-repo", "3rd-party", "testdata",
}
# Test-run artifacts and Python packaging metadata, neither of them written by
# hand: fio-mig-1787159565/, sbtest.egg-info/.
EXCLUDED_DIR_PATTERNS = (
    re.compile(r"^fio-mig-\d+$"),
    re.compile(r"\.egg-info$"),
)

# Generated Go files carry their marker in the first lines and are caught by
# is_generated(). These names are generated without one.
GENERATED_NAME_PATTERN = re.compile(r"^zz_generated|\.pb\.go$|_generated\.go$")

# Rules that need a path rather than a directory name, matched against the
# repository-relative path with "/" separators.
#
# helm-charts/charts/simplyblock-operator is the development chart: hand-written
# source, and checked like any other. What is excluded is everything around it
# that nobody edits by hand — the packaged releases next to it, the vendored
# subcharts, and the files "make helm-sync" copies out of the operator (those are
# fixed at their source, in the operator's markers and types).
EXCLUDED_PATH_PATTERNS = (
    # Published chart repositories: packaged .tgz releases and their index.
    re.compile(r"^helm-charts/charts/(?!simplyblock-operator(?:/|$))"),
    re.compile(r"^csi-driver/charts(?:/|$)"),
    # Vendored subcharts of the development chart.
    re.compile(r"^helm-charts/charts/simplyblock-operator/charts(?:/|$)"),
    # Written by make helm-sync from operator/config.
    re.compile(r"^helm-charts/charts/simplyblock-operator/crds(?:/|$)"),
    re.compile(r"^helm-charts/charts/simplyblock-operator/templates/roles(?:/|$)"),
    re.compile(
        r"^helm-charts/charts/simplyblock-operator/templates/"
        r"simplyblock-operator-webhook\.yaml$"
    ),
)

CODE_FENCE_PATTERN = re.compile(r"^\s*```")
# The info string of an opening fence, e.g. 'bash title="Create a cluster"'.
CODE_FENCE_TITLE_PATTERN = re.compile(r"title\s*=\s*\"([^\"]*)\"")

# A table needs its separator row, otherwise every row runs together as one
# paragraph.
TABLE_ROW_PATTERN = re.compile(r"^\s*\|")
TABLE_SEPARATOR_PATTERN = re.compile(r"^\s*\|[\s:|-]+\|\s*$")

# Regions that are not prose: inline code spans (a run of backticks closed by a
# run of the same length) and mkdocs-macros template expressions. The latter cover
# both placeholders declared under "extra" in mkdocs.yml ({{ cliname }}) and
# statements such as snippet includes ({% include 'file.md' %}).
CODE_SPAN_PATTERN = re.compile(r"(`+)(?:.+?)\1")
TEMPLATE_PATTERN = re.compile(r"\{\{.*?\}\}|\{%.*?%\}")

# Regions that carry no prose either, but that only the higher level checks mask:
# link and image targets, reference definitions, bare urls, attribute lists
# ({:target="_blank"}, {#anchor}) and inline html tags.
LINK_TARGET_PATTERN = re.compile(r"(?<=\])\([^)]*\)")
REFERENCE_TARGET_PATTERN = re.compile(r"^(\s*\[[^\]]+\]:).*$")
URL_PATTERN = re.compile(r"<?\b(?:https?|ftp)://\S+>?")
ATTRIBUTE_LIST_PATTERN = re.compile(r"\{[^{}]*\}")
HTML_TAG_PATTERN = re.compile(r"</?[a-zA-Z][^<>]*>")

# The comment of a code block line. A comment is written by the author of the
# page, the code around it is not. The space behind the marker keeps a shebang
# ("#!/bin/bash"), a url and a json pointer ("#/definitions") out. Only "#" and
# "//" are markers: ";" and "--" open a comment in some languages, but separate
# a command from its arguments in the shell.
CODE_COMMENT_PATTERN = re.compile(r"(?:^|\s)(?:#|//)\s")

# Raw HTML blocks start with a block level tag on their own line. Their content
# is only Markdown if the block carries the md_in_html "markdown" attribute.
HTML_BLOCK_OPEN_PATTERN = re.compile(r"^\s*<([a-zA-Z][\w:-]*)((?:\"[^\"]*\"|'[^']*'|[^>\"'])*)>")
MD_IN_HTML_ATTR_PATTERN = re.compile(
    r"(?:^|\s)markdown(?:\s*=\s*(?:\"[^\"]*\"|'[^']*'|\S+))?(?=\s|/|$)"
)
VOID_HTML_TAGS = {
    "area", "base", "br", "col", "embed", "hr", "img", "input",
    "link", "meta", "param", "source", "track", "wbr",
}
# Only a block level tag opens a raw HTML block, the same way mkdocs decides it.
# An inline element is written in the middle of a sentence and carries the prose
# with it, so a footnote that starts with "<sup>2</sup> Test setups require ..."
# is a sentence and not a block, and is checked like any other line.
INLINE_HTML_TAGS = {
    "a", "abbr", "b", "bdi", "bdo", "cite", "code", "data", "del", "dfn", "em",
    "i", "ins", "kbd", "mark", "q", "s", "samp", "small", "span", "strong",
    "sub", "sup", "time", "u", "var",
}
BLOCK_HTML_TAGS = {
    "address", "article", "aside", "blockquote", "button", "canvas", "caption",
    "colgroup", "dd", "details", "div", "dl", "dt", "fieldset", "figcaption",
    "figure", "footer", "form", "h1", "h2", "h3", "h4", "h5", "h6", "header",
    "iframe", "label", "legend", "li", "main", "nav", "noscript", "object",
    "ol", "optgroup", "option", "p", "picture", "pre", "script", "section",
    "select", "style", "summary", "svg", "table", "tbody", "td", "template",
    "textarea", "tfoot", "th", "thead", "tr", "ul", "video",
}
# Every name that is an HTML element. A "<" followed by anything else is not a
# tag but a placeholder, a shell redirect or a comparison ("<CLUSTER_ID>",
# "< 4 KiB"), and a check that pairs tags has to leave those alone.
HTML_ELEMENT_TAGS = VOID_HTML_TAGS | INLINE_HTML_TAGS | BLOCK_HTML_TAGS

# Elements whose body is not markup. Everything between the two is code, and a
# "<" inside it is an operator rather than the start of a tag.
HTML_RAW_TEXT_TAGS = {"script", "style", "textarea"}

# An HTML comment, which pairs on its own and holds no tags.
HTML_COMMENT_PATTERN = re.compile(r"<!--.*?-->", re.DOTALL)
# An opening or closing tag, with its name and the "/" of a self-closing one.
HTML_TAG_PAIR_PATTERN = re.compile(
    r"<(?P<closing>/?)(?P<tag>[a-zA-Z][\w:-]*)"
    r"(?P<attrs>(?:\"[^\"]*\"|'[^']*'|[^>\"'])*)>"
)

# Frontmatter fields that hold prose. The remaining fields are configuration
# (weight, redirects, ...) and are not written for a reader.
FRONTMATTER_FENCE = "---"
FRONTMATTER_PROSE_FIELDS = ("title", "description")
FRONTMATTER_FIELD_PATTERN = re.compile(r"^([A-Za-z_][\w-]*)\s*:\s*")

# Generated files are corrected at their source, so they are not checked.
GENERATED_MARKER_PATTERN = re.compile(
    r"this file is generated|do not edit (?:it )?by hand|code generated .*do not edit",
    re.IGNORECASE,
)
GENERATED_MARKER_LINES = 15

SEVERITY_ERROR = "error"
SEVERITY_WARNING = "warning"

# Every reported line opens with its severity, so that a finding stands out while
# the gates scroll past, and so that quality-gate.sh can collect the errors of all
# gates into one list at the end of a run.
ERROR_PREFIX = "ERROR  "
WARNING_PREFIX = "WARN   "


CONTEXT_PROSE = "prose"
CONTEXT_CODE = "code"


@dataclass
class ProseLine:
    """A single line of page text.

    "text" is the line as written, "masked" is the same line with every region
    that carries no text replaced by spaces. Both have the same length, so a
    column found in "masked" points at the same character in "text".

    "context" is CONTEXT_PROSE for the prose of a page, and CONTEXT_CODE for the
    body of a code block, which a check only sees when it asks for it.
    """

    number: int
    text: str
    masked: str
    context: str = CONTEXT_PROSE


@dataclass
class Violation:
    file: str
    line: int
    column: int
    check: str
    reason: str
    excerpt: str
    severity: str = SEVERITY_ERROR


@dataclass
class FileFix:
    """A replacement of "length" characters at a 1-based line and column."""

    line: int
    column: int
    length: int
    replacement: str


def repository_root():
    """The repository the gates run against.

    The scripts live under .claude/skills/house-style/scripts/, so the root is
    not the parent of the script directory. Git is asked first, and the upward
    walk is the fallback for a checkout without git.
    """
    override = os.environ.get("HOUSE_STYLE_ROOT")
    if override:
        return os.path.abspath(override)

    current = os.path.dirname(os.path.abspath(__file__))
    while True:
        if os.path.isdir(os.path.join(current, ".git")):
            return current
        parent = os.path.dirname(current)
        if parent == current:
            return os.path.abspath(
                os.path.join(
                    os.path.dirname(os.path.abspath(__file__)), "..", "..", "..", ".."
                )
            )
        current = parent


# The gate scripts carry every wrong spelling they look for, as the data of their
# own word lists, so they exclude themselves from a scan.
SELF_DIR = os.path.dirname(os.path.abspath(__file__))


def is_self(file_path):
    return os.path.dirname(os.path.abspath(file_path)) == SELF_DIR


def is_excluded_dir(name):
    return name in EXCLUDED_DIR_NAMES or any(
        pattern.match(name) for pattern in EXCLUDED_DIR_PATTERNS
    )


def relative_to_root(path):
    """The path as the exclusion rules see it: relative to the root, "/" joined."""
    return os.path.relpath(os.path.abspath(path), repository_root()).replace(os.sep, "/")


def is_excluded_relpath(path):
    return any(pattern.match(relative_to_root(path)) for pattern in EXCLUDED_PATH_PATTERNS)


def is_excluded_path(path):
    """Whether any directory along the path is excluded.

    walk() prunes as it descends, which covers a scan of a parent. This also
    catches an excluded directory (or a file inside one) that is passed
    explicitly, where there is nothing above it left to prune.
    """
    if is_excluded_relpath(path):
        return True
    parts = os.path.abspath(path).split(os.sep)
    if os.path.isfile(path):
        parts = parts[:-1]
    return any(is_excluded_dir(part) for part in parts if part)


def walk(directory):
    files = []
    for root, dirnames, filenames in os.walk(directory):
        dirnames[:] = sorted(
            name
            for name in dirnames
            if not is_excluded_dir(name)
            and not is_excluded_relpath(os.path.join(root, name))
        )
        for name in sorted(filenames):
            if os.path.splitext(name)[1].lower() not in SCANNED_EXTENSIONS:
                continue
            if GENERATED_NAME_PATTERN.search(name):
                continue
            path = os.path.join(root, name)
            if is_self(path) or is_excluded_relpath(path):
                continue
            files.append(path)
    return files


def read_lines(file_path):
    with open(file_path, "r", encoding="utf-8") as handle:
        content = handle.read()
    if content.startswith("﻿"):
        content = content[1:]
    return re.split(r"\r?\n", content)


def write_lines(file_path, lines):
    # read_lines() keeps a trailing empty element for a final newline, so joining
    # restores the file exactly as it was, including whether it ended with one.
    with open(file_path, "w", encoding="utf-8") as handle:
        handle.write("\n".join(lines))


def is_generated(file_path):
    """Detect the "do not edit by hand" marker that generators write out."""
    head = read_lines(file_path)[:GENERATED_MARKER_LINES]
    return any(GENERATED_MARKER_PATTERN.search(line) for line in head)


def indentation_of(line):
    return len(line) - len(line.lstrip())


def get_line_excerpt(line, col):
    start = max(0, col - 30)
    end = min(len(line), col + 45)
    return line[start:end].strip()


def collect_files(paths, default_dirs=DEFAULT_TARGET_DIRS, on_missing=None):
    """Resolve the paths to scan into a sorted list of Markdown files.

    The defaults are anchored to the repository root, so a check can be run from
    anywhere; explicitly passed paths stay relative to the current directory.
    """
    if paths:
        targets = [os.path.abspath(path) for path in paths]
    else:
        targets = [os.path.join(repository_root(), name) for name in default_dirs]

    files = []
    for target in targets:
        if is_excluded_path(target):
            continue
        if os.path.isdir(target):
            files.extend(walk(target))
        elif os.path.isfile(target):
            if not is_self(target):
                files.append(target)
        elif on_missing is not None:
            on_missing(target)
    return files


def drop_generated(files, report=None):
    """Remove the generated files, reporting them through "report" if given."""
    generated = [file for file in files if is_generated(file)]
    if not generated:
        return files
    if report is not None:
        report(generated)
    skipped = set(generated)
    return [file for file in files if file not in skipped]


def relative_path(file_path):
    rel = os.path.relpath(file_path, os.getcwd())
    return file_path if rel.startswith("..") else rel


# Characters that join words into an identifier, a path, a package name or a
# host: "my-cluster", "docs/kubernetes", "nvme-cli", "docker.io".
IDENTIFIER_CHARS = set("-_./\\:@=+$~")


def is_part_of_identifier(line, start, end):
    """Tell whether the match at (start, end) is part of a longer token.

    A separator only joins when a word continues behind it: "docker.io" is a
    host, "on Kubernetes." is the end of a sentence, and "(NVMe-oF)" is a word in
    brackets.
    """
    if start > 0 and line[start - 1] in IDENTIFIER_CHARS:
        # Nothing before the separator means the token starts with it, as a path
        # ("/etc/nvme") or an option ("--nvme") does.
        if start < 2 or line[start - 2].isalnum() or line[start - 2] == "_":
            return True

    if end < len(line) and line[end] in IDENTIFIER_CHARS:
        following = line[end + 1] if end + 1 < len(line) else ""
        if following.isalnum() or following == "_":
            return True

    return False


def template_ranges(line):
    return [match.span() for match in TEMPLATE_PATTERN.finditer(line)]


def non_prose_ranges(line):
    """Return the (start, end) ranges of the inline code and template regions."""
    ranges = [match.span() for match in CODE_SPAN_PATTERN.finditer(line)]
    ranges.extend(match.span() for match in TEMPLATE_PATTERN.finditer(line))
    return ranges


def is_inside_range(ranges, index):
    return any(start <= index < end for start, end in ranges)


def mask_ranges(line, ranges):
    """Blank out the given regions, keeping the length of the line."""
    if not ranges:
        return line
    chars = list(line)
    for start, end in ranges:
        for index in range(start, min(end, len(chars))):
            chars[index] = " "
    return "".join(chars)


def mask_non_prose(line):
    """Blank out every region of a line that is not prose.

    Next to inline code and template expressions this covers link and image
    targets, bare urls, attribute lists and inline html tags. The link text
    itself is prose and stays, as does the alt text of an image.
    """
    ranges = non_prose_ranges(line)
    masked = mask_ranges(line, ranges)

    for pattern in (
        URL_PATTERN,
        LINK_TARGET_PATTERN,
        ATTRIBUTE_LIST_PATTERN,
        HTML_TAG_PATTERN,
    ):
        ranges.extend(match.span() for match in pattern.finditer(masked))
        masked = mask_ranges(line, ranges)

    reference = REFERENCE_TARGET_PATTERN.match(masked)
    if reference:
        ranges.append((len(reference.group(1)), len(line)))
        masked = mask_ranges(line, ranges)

    return masked


def frontmatter_value_span(line):
    """Return the (start, end) span of a frontmatter value, without its quotes."""
    match = FRONTMATTER_FIELD_PATTERN.match(line)
    if not match:
        return None
    start, end = match.end(), len(line.rstrip())
    if end - start >= 2 and line[start] in "\"'" and line[end - 1] == line[start]:
        start, end = start + 1, end - 1
    return start, end


def iter_prose_lines(lines, frontmatter_fields=FRONTMATTER_PROSE_FIELDS, include_code=False):
    """Yield a ProseLine for every line of a page that carries prose.

    Skipped are the code blocks, the raw HTML blocks without an md_in_html
    "markdown" attribute, and the frontmatter fields that are not prose. Of an
    opening code fence only its title attribute is prose, of a frontmatter field
    only its value.

    With "include_code" the body of a code block is yielded as well, marked as
    CONTEXT_CODE. A check that looks at it has to treat it more leniently: a code
    block holds commands, values and identifiers, not sentences.
    """
    in_frontmatter = False
    frontmatter_fence_count = 0
    in_code_fence = False
    html_skip_tag = None
    html_skip_depth = 0

    for index, line in enumerate(lines):
        trimmed = line.strip()

        # Leading blank lines before the frontmatter fence are tolerated.
        if frontmatter_fence_count == 0 and index <= 3 and trimmed == FRONTMATTER_FENCE:
            in_frontmatter = True
            frontmatter_fence_count = 1
            continue

        if in_frontmatter:
            if trimmed == FRONTMATTER_FENCE:
                frontmatter_fence_count += 1
                if frontmatter_fence_count >= 2:
                    in_frontmatter = False
                continue
            field = FRONTMATTER_FIELD_PATTERN.match(line)
            if not field or field.group(1) not in frontmatter_fields:
                continue
            start, end = frontmatter_value_span(line)
            masked = mask_ranges(line, [(0, start), (end, len(line))])
            yield ProseLine(number=index + 1, text=line, masked=mask_non_prose(masked))
            continue

        if CODE_FENCE_PATTERN.match(line):
            in_code_fence = not in_code_fence
            # An opening fence may carry a title attribute
            # (```bash title="Deploy a cluster"), which is prose.
            title = CODE_FENCE_TITLE_PATTERN.search(line) if in_code_fence else None
            if title:
                masked = mask_ranges(
                    line, [(0, title.start(1)), (title.end(1), len(line))]
                )
                yield ProseLine(number=index + 1, text=line, masked=masked)
            continue
        if in_code_fence:
            comment = CODE_COMMENT_PATTERN.search(line) if include_code else None
            if comment:
                # Commands, values and program output are literals. Only the
                # comment next to them is written as text.
                masked = mask_ranges(line, [(0, comment.end()), *template_ranges(line)])
                yield ProseLine(
                    number=index + 1, text=line, masked=masked, context=CONTEXT_CODE
                )
            continue

        # Raw HTML blocks are exempt, unless they carry the md_in_html "markdown"
        # attribute. With that attribute set, mkdocs renders the block content as
        # Markdown, so it is prose like any other. Inline HTML in a prose line
        # does not open a block.
        if html_skip_tag is None:
            open_match = HTML_BLOCK_OPEN_PATTERN.match(line)
            if open_match and not MD_IN_HTML_ATTR_PATTERN.search(open_match.group(2)):
                tag = open_match.group(1).lower()
                if tag in INLINE_HTML_TAGS:
                    # An inline element opens no block. The line is prose, and
                    # the tag around it is masked like any other inline HTML.
                    pass
                elif tag in VOID_HTML_TAGS or open_match.group(2).rstrip().endswith("/"):
                    # Self-contained element, no block to skip over.
                    continue
                else:
                    html_skip_tag = tag
                    html_skip_depth = 0
        if html_skip_tag is not None:
            html_skip_depth += len(
                re.findall(rf"<{re.escape(html_skip_tag)}\b", line, re.IGNORECASE)
            )
            html_skip_depth -= len(
                re.findall(rf"</{re.escape(html_skip_tag)}\s*>", line, re.IGNORECASE)
            )
            if html_skip_depth <= 0:
                html_skip_tag = None
            continue

        if not trimmed:
            continue

        yield ProseLine(number=index + 1, text=line, masked=mask_non_prose(line))


# ─── Prose in a source file ────────────────────────────────────────────────
#
# Markdown carries prose directly. A Go, Python, or YAML file carries it in its
# comments and docstrings, and the code around them is not written for a reader:
# an identifier is not a misspelling, a struct tag is not punctuation, and a
# string literal is a value.
#
# Rather than teaching every gate a second syntax, a source file is reduced to
# the shape of a Markdown file: every character that is not prose is replaced by
# a space, while the line count and every column stay as they are. A gate then
# walks the result exactly as it walks a page, and a --fix writes back into the
# real file at the column it reported.

COMMENT_DIRECTIVE_PATTERN = re.compile(
    r"""^\s*(?:
          \+                        # kubebuilder and k8s markers: +optional, +kubebuilder:
        | go:                       # //go:generate, //go:build
        | nolint
        | noqa
        | type:\s
        | pylint:
        | pyright:
        | mypy:
        | fmt:\s*(?:on|off)
        | pragma:
        | ruff:
        | yamllint\b
        | yaml-language-server:
        | -\*-                      # coding declarations
        | Code\ generated\b
    )""",
    re.VERBOSE | re.IGNORECASE,
)

BLOCK_COMMENT_MARGIN_PATTERN = re.compile(r"^(\s*)\*(\s|$)")

# A comment line whose text is indented behind its marker is a verbatim block,
# not a sentence: godoc renders a tab-indented line as preformatted (the package
# index in atlas-lib/doc.go is one), and a shell or YAML header indents its usage
# lines the same way. Package names, commands, and aligned columns are not prose,
# so the line is skipped rather than reported for spelling and spacing.
VERBATIM_COMMENT_PATTERN = re.compile(r"^(?:\t| {3,})\S")
YAML_DOCUMENT_MARKER_PATTERN = re.compile(r"^\s*(?:---|\.\.\.)\s*$")

# A commented-out line of configuration is code, not prose. In a values file or
# an example manifest most comments are exactly that:
#
#   # qos:
#   #   iops: 10000
#   #   - "0000:02:00.0"
#   #   pcieModel: "INTEL SSDPE2KX010T8"
#
# A mapping key on its own, a list item, or a key whose value is a single scalar
# is treated as configuration. A key followed by several words is prose, so
# "# Note: the operator requires a pinned volume" is still checked.
YAML_SCALAR = r'''(?:"[^"]*"|\'[^\']*\'|\S+)'''
COMMENTED_OUT_YAML_PATTERN = re.compile(
    r"^\s*(?:-\s+" + YAML_SCALAR + r"|[A-Za-z_][\w.-]*:(?:\s*" + YAML_SCALAR + r")?)\s*$"
)

QUOTE_CHARS = "\"'"


def syntax_of(file_path):
    """The syntax of a file, by extension. An unknown extension is Markdown."""
    return SYNTAX_OF_EXTENSION.get(os.path.splitext(file_path)[1].lower(), "markdown")


def _blank(length):
    return " " * length


def _keep(source, start, end):
    """A line of spaces with source[start:end] left in place."""
    end = max(start, min(end, len(source)))
    return _blank(start) + source[start:end] + _blank(len(source) - end)


def _merge(base, addition):
    """Overlay the kept characters of "addition" onto "base"."""
    return "".join(
        addition[index] if addition[index] != " " else base[index]
        for index in range(len(base))
    )


def _without_directive(kept, start):
    """Blank a comment out entirely when it is a directive rather than prose."""
    if COMMENT_DIRECTIVE_PATTERN.match(kept[start:]):
        return _blank(len(kept))
    return kept


def _skip_quoted(line, index):
    """Advance past a quoted string that starts at index."""
    quote = line[index]
    index += 1
    while index < len(line):
        if line[index] == "\\":
            index += 2
            continue
        if line[index] == quote:
            return index + 1
        index += 1
    return index


def _mask_go(lines):
    """Keep the // and /* */ comments of a Go file and drop everything else."""
    masked = []
    in_block = False

    for line in lines:
        if in_block:
            end = line.find("*/")
            if end == -1:
                margin = BLOCK_COMMENT_MARGIN_PATTERN.match(line)
                start = margin.end() if margin else 0
                masked.append(_without_directive(_keep(line, start, len(line)), start))
            else:
                masked.append(_keep(line, 0, end))
                in_block = False
            continue

        index = 0
        result = _blank(len(line))
        while index < len(line):
            char = line[index]
            if char in QUOTE_CHARS or char == "`":
                if char == "`":
                    index += 1
                    while index < len(line) and line[index] != "`":
                        index += 1
                    index += 1
                else:
                    index = _skip_quoted(line, index)
                continue
            if line.startswith("//", index):
                if VERBATIM_COMMENT_PATTERN.match(line[index + 2:]):
                    break
                result = _merge(
                    result,
                    _without_directive(_keep(line, index + 2, len(line)), index + 2),
                )
                break
            if line.startswith("/*", index):
                end = line.find("*/", index + 2)
                if end == -1:
                    result = _merge(
                        result,
                        _without_directive(_keep(line, index + 2, len(line)), index + 2),
                    )
                    in_block = True
                    break
                result = _merge(result, _keep(line, index + 2, end))
                index = end + 2
                continue
            index += 1
        masked.append(result)

    return masked


def _mask_python(lines):
    """Keep the # comments and the triple-quoted strings of a Python file."""
    masked = []
    delimiter = None

    for line in lines:
        if delimiter is not None:
            end = line.find(delimiter)
            if end == -1:
                masked.append(_keep(line, 0, len(line)))
            else:
                masked.append(_keep(line, 0, end))
                delimiter = None
            continue

        index = 0
        result = _blank(len(line))
        while index < len(line):
            triple = line[index:index + 3]
            if triple in ('"""', "'''"):
                end = line.find(triple, index + 3)
                if end == -1:
                    result = _merge(result, _keep(line, index + 3, len(line)))
                    delimiter = triple
                    break
                result = _merge(result, _keep(line, index + 3, end))
                index = end + 3
                continue

            char = line[index]
            if char in QUOTE_CHARS:
                index = _skip_quoted(line, index)
                continue
            if char == "#":
                if index == 0 and line.startswith("#!"):
                    break
                if VERBATIM_COMMENT_PATTERN.match(line[index + 1:]):
                    break
                result = _merge(
                    result,
                    _without_directive(_keep(line, index + 1, len(line)), index + 1),
                )
                break
            index += 1
        masked.append(result)

    return masked


def _mask_yaml(lines):
    """Keep the # comments of a YAML file.

    A value is configuration rather than prose, and the description fields of a
    generated CRD are written by its generator. Only the comments are text.
    """
    masked = []

    for line in lines:
        if YAML_DOCUMENT_MARKER_PATTERN.match(line):
            masked.append(_blank(len(line)))
            continue

        index = 0
        result = _blank(len(line))
        while index < len(line):
            char = line[index]
            if char in QUOTE_CHARS:
                index = _skip_quoted(line, index)
                continue
            if char == "#":
                if index == 0 and line.startswith("#!"):
                    break
                text = line[index + 1:]
                if COMMENTED_OUT_YAML_PATTERN.match(text) or VERBATIM_COMMENT_PATTERN.match(text):
                    # Commented-out configuration or an indented usage block.
                    break
                result = _merge(
                    result,
                    _without_directive(_keep(line, index + 1, len(line)), index + 1),
                )
                break
            index += 1
        masked.append(result)

    return masked


MASKERS = {"go": _mask_go, "python": _mask_python, "yaml": _mask_yaml}


def prose_source_lines(file_path, lines=None):
    """Read a file and return the lines a prose gate should walk.

    A Markdown file is returned as it is. A source file is returned with only
    its comments and docstrings left standing, so that a gate sees prose and
    nothing else while every line number and column still refers to the file on
    disk.
    """
    if lines is None:
        lines = read_lines(file_path)
    masker = MASKERS.get(syntax_of(file_path))
    if masker is None:
        return lines
    # A comment that happens to read as "---" would open a frontmatter block for
    # a gate that walks the result as Markdown. In a source file there is no
    # frontmatter, so the line is blanked out.
    return [
        " " * len(line) if line.strip() == FRONTMATTER_FENCE else line
        for line in masker(lines)
    ]


# ─── Code in a source file ─────────────────────────────────────────────────
#
# The mirror image of prose_source_lines(): the comments, the docstrings, and the
# string literals are blanked out, and the code is what stays. A gate that checks
# the names a declaration introduces reads this instead of the prose.
#
# A Go raw string survives, because a struct tag lives in one and the field name
# it declares is part of the API: `json:"behaviorMode"`.


def _mask_code_go(lines):
    """Keep the code of a Go file, drop its comments and quoted strings."""
    masked = []
    in_block = False

    for line in lines:
        result = list(line)

        def blank(start, end):
            for index in range(start, min(end, len(result))):
                result[index] = " "

        if in_block:
            end = line.find("*/")
            if end == -1:
                masked.append(_blank(len(line)))
                continue
            # The block ends on this line, the code behind it counts again.
            in_block = False
            blank(0, end + 2)
            index = end + 2
        else:
            index = 0

        while index < len(line):
            char = line[index]
            if char in QUOTE_CHARS:
                end = _skip_quoted(line, index)
                # The quotes stay, so that a tag or a literal is still delimited.
                blank(index + 1, end - 1)
                index = end
                continue
            if char == "`":
                # A raw string carries the struct tags, which declare API names.
                index += 1
                while index < len(line) and line[index] != "`":
                    index += 1
                index += 1
                continue
            if line.startswith("//", index):
                blank(index, len(line))
                break
            if line.startswith("/*", index):
                end = line.find("*/", index + 2)
                if end == -1:
                    blank(index, len(line))
                    in_block = True
                    break
                blank(index, end + 2)
                index = end + 2
                continue
            index += 1

        masked.append("".join(result))

    return masked


def _mask_code_python(lines):
    """Keep the code of a Python file, drop its comments and strings."""
    masked = []
    delimiter = None

    for line in lines:
        result = list(line)

        def blank(start, end):
            for index in range(start, min(end, len(result))):
                result[index] = " "

        if delimiter is not None:
            end = line.find(delimiter)
            if end == -1:
                masked.append(_blank(len(line)))
                continue
            # The string ends on this line, the code behind it counts again.
            blank(0, end + len(delimiter))
            index = end + len(delimiter)
            delimiter = None
        else:
            index = 0

        while index < len(line):
            triple = line[index:index + 3]
            if triple in ('"""', "'''"):
                end = line.find(triple, index + 3)
                if end == -1:
                    blank(index, len(line))
                    delimiter = triple
                    break
                blank(index, end + 3)
                index = end + 3
                continue
            char = line[index]
            if char in QUOTE_CHARS:
                end = _skip_quoted(line, index)
                blank(index + 1, end - 1)
                index = end
                continue
            if char == "#":
                blank(index, len(line))
                break
            index += 1

        masked.append("".join(result))

    return masked


def _mask_code_yaml(lines):
    """Keep the keys and the structure of a YAML file, drop its comments."""
    masked = []

    for line in lines:
        result = list(line)
        index = 0

        while index < len(line):
            char = line[index]
            if char in QUOTE_CHARS:
                end = _skip_quoted(line, index)
                for position in range(index + 1, min(end - 1, len(result))):
                    result[position] = " "
                index = end
                continue
            if char == "#":
                for position in range(index, len(result)):
                    result[position] = " "
                break
            index += 1

        masked.append("".join(result))

    return masked


CODE_MASKERS = {
    "go": _mask_code_go,
    "python": _mask_code_python,
    "yaml": _mask_code_yaml,
}


def code_source_lines(file_path, lines=None):
    """Read a file and return its lines with only the code left standing.

    A Markdown file has no code to declare names in and yields nothing.
    """
    if lines is None:
        lines = read_lines(file_path)
    masker = CODE_MASKERS.get(syntax_of(file_path))
    return [] if masker is None else masker(lines)


def apply_fixes_to_file(file_path, fixes):
    """Apply the replacements to a file and return how many were written."""
    if not fixes:
        return 0

    lines = read_lines(file_path)

    grouped = {}
    for fix in fixes:
        grouped.setdefault(fix.line, []).append(fix)

    applied = 0
    for line_number, line_fixes in grouped.items():
        line_index = line_number - 1
        if line_index >= len(lines):
            continue
        updated = lines[line_index]
        for fix in sorted(line_fixes, key=lambda f: f.column, reverse=True):
            start = fix.column - 1
            end = start + fix.length
            updated = f"{updated[:start]}{fix.replacement}{updated[end:]}"
            applied += 1
        lines[line_index] = updated

    write_lines(file_path, lines)
    return applied


def report_violations(violations, check_name, files, success_message, group_warnings=True):
    """Print the report of a gate and return its exit code.

    Warnings are grouped per file by default: they are numerous and only their
    line numbers are needed to find them. A gate whose warnings are candidates to
    read rather than places to visit passes group_warnings=False, so that every
    one is printed with its reason and its excerpt.
    """
    errors = [v for v in violations if v.severity == SEVERITY_ERROR]
    warnings = [v for v in violations if v.severity == SEVERITY_WARNING]

    if errors:
        print(f"{check_name} failed with {len(errors)} error(s):", file=sys.stderr)
        for v in errors:
            # The "ERROR" token opens the line, so that a finding is obvious while
            # the gates scroll past and can be collected again afterwards.
            print(
                f"{ERROR_PREFIX} {v.file}:{v.line}:{v.column} | {v.check} | {v.reason}\n"
                f"{' ' * (len(ERROR_PREFIX) + 1)}{v.excerpt}",
                file=sys.stderr,
            )
        sys.stderr.flush()

    if warnings:
        print(f"\n{len(warnings)} warning(s), these do not fail the check yet:")
        if group_warnings:
            grouped = {}
            for v in warnings:
                grouped.setdefault((v.file, v.check), []).append(v.line)
            for (file, check), numbers in grouped.items():
                print(
                    f"{WARNING_PREFIX} {file} | {check} | "
                    f"line(s) {', '.join(str(n) for n in numbers)}"
                )
        else:
            for v in warnings:
                print(
                    f"{WARNING_PREFIX} {v.file}:{v.line}:{v.column} | {v.reason}\n"
                    f"{' ' * (len(WARNING_PREFIX) + 1)}{v.excerpt}"
                )
        sys.stdout.flush()

    if errors:
        return 1

    print(success_message.format(files=len(files), warnings=len(warnings)))
    return 0
