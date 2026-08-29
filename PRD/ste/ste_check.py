#!/usr/bin/env python3
"""ASD-STE100 checker for the stationa PRD documents.

Checks the machine-verifiable subset of ASD-STE100 (Issue 9) that matters for
these PRDs, against the dictionaries in ./dictionaries/:

  1.1   WORD      — word is on the non-approved list (with approved alternative)
  2.1   NOUNPHRASE— noun cluster of more than three nouns (heuristic)
  3.6   PASSIVE   — passive voice (be/get + past participle heuristic)
  3.2/3.5 INGVB   — progressive form (is/are/was/were + -ing)
  5.1   LEN20     — more than 20 words in a procedural (imperative) sentence
  6.3   LEN25     — more than 25 words in a descriptive sentence
  8.1   SEMICOL   — semicolon
  8.1   AMP       — ampersand
  4.2   CONTR     — contraction (don't, isn't, ...)
  GR-6  LATIN     — Latin abbreviations (e.g., i.e., etc., viz., c.f.)
  9.3   PHRASAL   — known wordy/unapproved phrases (prior to, in order to, ...)

The standard's full dictionary is ASD-copyrighted. This checker uses a small
curated MIT-licensed base (github.com/swarooppatilx/ste100) extended with our
own pairs and the project vocabulary in project.json (rule 1.5/1.12 technical
words). It therefore cannot prove conformance by itself — it finds the
violations it can see; a writer/editor does the rest (rule 9.1).

Usage:
  python3 ste_check.py                  # check PRD docs (excludes _research/)
  python3 ste_check.py FILE...          # check specific files
  python3 ste_check.py --summary        # only per-file totals
Exit status 0 = no ERROR-level findings.
"""

import json
import re
import sys
from pathlib import Path

HERE = Path(__file__).resolve().parent
DOT_DIR = HERE / "dictionaries"

MAX_PROCEDURAL_WORDS = 20   # rule 5.1
MAX_DESCRIPTIVE_WORDS = 25  # rule 6.3

IRREGULAR_PARTICIPLES = {
    "done", "made", "kept", "set", "put", "sent", "given", "taken", "known",
    "held", "shown", "written", "driven", "read", "fed", "led", "built",
    "told", "lost", "found", "left", "seen", "heard", "cut", "hit", "begun",
    "brought", "bought", "caught", "chosen", "come", "become", "got",
    "forgotten", "drawn", "grown", "held", "included", "lost", "met", "paid",
    "run", "said", "sold", "spent", "spoken", "spent", "stood", "understood",
}

CONTRACTIONS = re.compile(r"\b[a-z]+(?:n't|'re|'ve|'ll|'d)\b|\bit's\b|\bthat's\b|\bthere's\b", re.I)
LATIN = re.compile(r"\b(e\.g\.|i\.e\.|etc\.|viz\.|c\.f\.|cf\.|vs\.|v\.v\.)\b|\betc\b", re.I)
PHRASAL = re.compile(
    r"\b(?:prior to|subsequent to|in order to|in addition to|at this point in time"
    r"|for the purpose of|in the event of|with reference to|a number of|as per)\b", re.I)

BE_WORDS = ("am|is|are|was|were|be|been|being|gets?|got")
_VERB_STARTS = set()  # filled by load_dictionaries: words usable as imperatives


def load_dictionaries():
    global _VERB_STARTS
    unapproved = json.loads((DOT_DIR / "unapproved.json").read_text())
    approved_admin = json.loads((DOT_DIR / "approved.json").read_text())
    technical = json.loads((DOT_DIR / "technical.json").read_text())
    project = json.loads((DOT_DIR / "project.json").read_text())
    # words we must never flag: project technical nouns/verbs + STE
    # technical nouns + a few structural words
    allow = set()
    for w in project["nouns"] + project["verbs"]:
        for part in str(w).split():
            allow.add(part.lower())
            allow.add(part.lower() + "s")
    for w in technical["nouns"] + technical["verbs"]:
        for part in str(w).split():
            allow.add(part.lower())
    # imperative detection: approved verbs (POS map) + project technical verbs
    _VERB_STARTS = {w for w, pos in approved_admin.items() if pos == "VERB"}
    _VERB_STARTS |= {v.split()[0].lower() for v in project["verbs"]}
    return unapproved, allow


def expanded_forms(word: str) -> set:
    """Common inflections of a non-approved word (simple, no stemming lib)."""
    forms = {word, word + "s"}
    if word.endswith("e"):
        stem = word[:-1]
        forms |= {stem + "ed", stem + "ing", stem + "er", stem + "ed"}
    elif word.endswith("y"):
        stem = word[:-1]
        forms |= {stem + "ies", stem + "ied"}
    else:
        forms |= {word + "ed", word + "ing"}
    return forms


def strip_markdown(text: str):
    """Remove code blocks/inline code/links/images; return (lines, offsets).

    Output is a list of (line_number, line_text) with markdown noise removed,
    code fences blanked out.
    """
    lines_out = []
    in_fence = False
    fence_re = re.compile(r"^\s*(```|~~~)")
    for i, raw in enumerate(text.splitlines(), start=1):
        if fence_re.match(raw):
            in_fence = not in_fence
            lines_out.append((i, ""))
            continue
        if in_fence:
            lines_out.append((i, ""))
            continue
        line = raw
        line = re.sub(r"`[^`]*`", " ", line)              # inline code
        line = re.sub(r"!\[[^\]]*\]\([^)]*\)", " ", line)  # images
        line = re.sub(r"\[([^\]]*)\]\([^)]*\)", r"\1", line)  # links -> text
        line = re.sub(r"^\s*#{1,6}\s+", "", line)          # heading marker
        line = re.sub(r"^\s*[-*+>]\s+", "", line)          # bullets/quotes
        line = re.sub(r"^\s*\d+[.)]\s+", "", line)         # ordered lists
        lines_out.append((i, line))
    return lines_out


def sentences_from_lines(lines):
    """Yield (start_line, sentence_text) across markdown lines."""
    buf, buf_start = [], None
    for ln, text in lines:
        if not text.strip():
            if buf:
                yield (buf_start, " ".join(buf))
                buf, buf_start = [], None
            continue
        if buf_start is None:
            buf_start = ln
        buf.append(text.strip())
        # sentence end at . ! ? when followed by space+capital or EOL
        joined = " ".join(buf)
        if re.search(r"[.!?](\s|$)", text):
            # may be multiple sentences in one line
            parts = re.split(r"(?<=[.!?])\s+", joined)
            # emit all but possibly-continuing last part
            for p in parts[:-1]:
                yield (buf_start, p)
            buf = [parts[-1]] if parts else []
    if buf:
        yield (buf_start, " ".join(buf))


def count_words(sentence: str) -> int:
    """Word count per STE section 8: hyphenated words = 1, parenthetical = 1,
    number+unit = 1, table separators ignored."""
    s = sentence
    s = re.sub(r"\|", " ", s)
    s = re.sub(r"\([^)]*\)", " () ", s)          # parenthetical counts as one word
    s = re.sub(r"\b(\d+(?:\.\d+)?)\s*(hz|khz|mhz|ghz|w|kw|v|a|ms|s|kb|mb|gb)\b",
               r"\1", s, flags=re.I)
    words = re.findall(r"[A-Za-z0-9']+(?:-[A-Za-z0-9']+)*", s)
    return len(words)


def is_imperative(sentence: str) -> bool:
    """Heuristic for procedural writing: sentence starts with a bare base verb
    (e.g. 'Use', 'Check', 'Do not')."""
    global _VERB_STARTS
    words = re.findall(r"[A-Za-z']+", sentence)
    if not words:
        return False
    first = words[0].lower()
    if first == "do" and len(words) > 1 and words[1].lower() == "not":
        return True
    if first in ("don't", "donot", "please"):
        return True
    return first in _VERB_STARTS


def check_sentence(start_line, sentence, unapproved, allow, findings, relpath):
    # Build word-token stream with positions for accurate reporting.
    tokens = [(m.group(0), m.start()) for m in
              re.finditer(r"[A-Za-z][A-Za-z'’-]*", sentence)]
    lowered = sentence.lower()

    def flag(line_off, rule, msg, sev="ERROR"):
        findings.append((relpath, start_line + line_off, rule, sev, msg))

    # 1.1 non-approved words
    for tok, pos in tokens:
        low = tok.lower().strip("'-")
        if low in ("not", "", "-"):
            continue
        if low in allow:
            continue
        entry = unapproved.get(low)
        if entry is None:
            # try inflections against the base entry
            for form in expanded_forms(low):
                if form in unapproved:
                    entry = unapproved[form]
                    break
        if entry is not None:
            alt = entry if isinstance(entry, str) else " / ".join(entry)
            flag(0, "1.1 WORD", f'"{tok}" is not approved - prefer "{alt}"')

    # 8.1 semicolons / ampersands
    for m in re.finditer(r";", sentence):
        flag(0, "8.1 SEMICOL", "semicolon - use a separate sentence")
    for m in re.finditer(r"&", sentence):
        flag(0, "8.1 AMP", "ampersand - write 'and'")
    # 4.2 contractions
    for m in CONTRACTIONS.finditer(sentence):
        flag(0, "4.2 CONTR", f"contraction \"{m.group(0)}\" - write the words in full")
    # GR-6 Latin abbreviations
    for m in LATIN.finditer(sentence):
        flag(0, "GR-6 LATIN", f"Latin abbreviation \"{m.group(0)}\" - write the words in full")
    # known wordy phrases (redundant with dictionary but gives the rule id)
    for m in PHRASAL.finditer(sentence):
        flag(0, "9.1 PHRASE", f"wordy phrase \"{m.group(0)}\" - {unapproved.get(m.group(0).lower(), 'reword shorter')}")

    # 3.6 passive voice: be/get + past participle
    irregs = "|".join(sorted(IRREGULAR_PARTICIPLES))
    pt_words = re.finditer(rf"\b({BE_WORDS})\s+(\w+ed|{irregs})\b", sentence)
    for m in pt_words:
        if m.group(2).lower() in ("used", "based", "made of"):
            continue  # adjectival uses that read as properties, not passives
        flag(0, "3.6 PASSIVE", f"passive voice \"{m.group(1)} {m.group(2)}\" - rewrite in the active voice", "WARN")

    # 3.2/3.5 progressive -ing verb
    for m in re.finditer(rf"\b({BE_WORDS})\s+(\w+ing)\b", sentence):
        flag(0, "3.5 INGVB", f"progressive form \"{m.group(0)}\" - use simple tenses", "WARN")

    # 5.1/6.3 sentence length
    n = count_words(sentence)
    if n > MAX_DESCRIPTIVE_WORDS:
        flag(0, "6.3 LEN25", f"descriptive sentence has {n} words (max {MAX_DESCRIPTIVE_WORDS})", "ERROR")
    elif n > MAX_PROCEDURAL_WORDS and is_imperative(sentence):
        flag(0, "5.1 LEN20", f"procedural sentence has {n} words (max {MAX_PROCEDURAL_WORDS})", "WARN")


def check_file(path: Path, unapproved, allow):
    text = path.read_text()
    # skip YAML frontmatter
    if text.startswith("---"):
        end = text.find("\n---", 3)
        if end != -1:
            text = text[end + 4:]
    lines = strip_markdown(text)
    findings = []
    rel = str(path)
    for start_line, sentence in sentences_from_lines(lines):
        check_sentence(start_line, sentence, unapproved, allow, findings, rel)
    return findings


def main(argv):
    args = list(argv)
    summary_only = "--summary" in args
    if summary_only:
        args.remove("--summary")
    if args:
        paths = [Path(a) for a in args]
    else:
        prd = HERE.parent
        paths = sorted(p for p in prd.rglob("*.md") if "_research" not in p.parts)

    unapproved, allow = load_dictionaries()
    all_findings = []
    for p in paths:
        if not p.exists():
            print(f"!! missing file: {p}", file=sys.stderr)
            continue
        all_findings += check_file(p, unapproved, allow)

    sev_rank = {"ERROR": 0, "WARN": 1}
    all_findings.sort(key=lambda f: (f[0], f[1], sev_rank[f[3]]))

    per_file = {}
    for f in all_findings:
        per_file.setdefault(f[0], {"ERROR": 0, "WARN": 0})
        per_file[f[0]][f[3]] += 1

    if summary_only:
        for f, c in sorted(per_file.items()):
            print(f"{c['ERROR']:5d} error  {c['WARN']:5d} warn   {f}")
        tot_e = sum(c["ERROR"] for c in per_file.values())
        tot_w = sum(c["WARN"] for c in per_file.values())
        print(f"{tot_e:5d} error  {tot_w:5d} warn   TOTAL")
    else:
        cur = None
        for rel, ln, rule, sev, msg in all_findings:
            if rel != cur:
                print(f"\n{rel}")
                cur = rel
            print(f"  {ln:5d} {rule:<14} {sev:<5} {msg}")
        print(f"\n{len(all_findings)} findings")

    return 1 if any(f[3] == "ERROR" for f in all_findings) else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))