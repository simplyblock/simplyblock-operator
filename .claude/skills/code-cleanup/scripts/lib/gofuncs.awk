# Emits one record per top-level Go function: how long it is, how deeply it
# indents, how many branches it carries, where it starts, what it is called, and
# a normalized copy of its body.
#
# It lives here because three of the cleanup passes ask the same question of a Go
# file — how big, how nested, how many copies — and a single pass over the source
# answers all three. Both measure.sh and find-twins.sh read these records.
#
# gofmt puts the closing brace of a top-level declaration in column 0, and that
# is what delimits a function below. The input is assumed gofmt-clean, which the
# build enforces.
#
# Output, tab-separated:
#   length  maxIndent  branches  file:line  signature  normalizedBody
#
# The normalized body drops blank lines, comment-only lines, and leading and
# trailing whitespace, so two copies that differ only in formatting or in the
# comments between their statements come out byte-identical and sort together.
#
# maxIndent counts leading tabs, so it also counts composite literals and
# wrapped call arguments, not only nested blocks. It is a hint, not a verdict:
# gocyclo is the authority on complexity, and branches below is the cheap proxy
# for what a reader has to hold at once.

function emit(len) {
    printf "%d\t%d\t%d\t%s:%d\t%s\t%s\n", len, maxindent, branches, file, start, sig, body
}

function begin() {
    file = FILENAME
    start = FNR
    sig = $0
    body = ""
    maxindent = 0
    branches = 0
}

FNR == 1 { infunc = 0 }

/^func / {
    # A second `func` while one is open means the previous declaration had no
    # body (a stub) and was never closed by a column-0 brace. Drop it rather
    # than swallowing everything up to the next one.
    begin()

    # A complete single-line function: `func f() int { return 1 }`.
    if ($0 ~ /\{.*\}[ \t]*$/) {
        emit(1)
        infunc = 0
        next
    }

    infunc = 1
    next
}

infunc {
    if ($0 ~ /^\}/) {
        emit(FNR - start + 1)
        infunc = 0
        next
    }

    indent = 0
    while (substr($0, indent + 1, 1) == "\t") {
        indent++
    }
    if (indent > maxindent) {
        maxindent = indent
    }

    stmt = $0
    gsub(/^[ \t]+/, "", stmt)
    gsub(/[ \t]+$/, "", stmt)
    if (stmt == "" || stmt ~ /^\/\//) {
        next
    }

    if (stmt ~ /^(if|for|switch|select)[ \t(]/ || stmt ~ /^\} else/ || stmt ~ /^case /) {
        branches++
    }
    branches += gsub(/&&/, "&&", stmt)
    branches += gsub(/\|\|/, "||", stmt)

    body = body ";" stmt
}
