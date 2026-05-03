#!/usr/bin/env bash
# Build a flat wiki layout from docs/.
#
# GitHub wiki only renders pages at the flat top level — nested paths like
# wiki/server/README return 404, and multiple files named README.md collide.
# This script generates a wiki-staging/ tree where every page is a flat
# hyphenated filename (Server.md, Plugins.md, Server-Components-Diagrams.md, …)
# and every .puml is rendered to an .svg embedded in the relevant index page.
#
# The script is idempotent and side-effect-free outside the output directory.
# Source docs/ is read-only; rendered .svg artifacts go into the staging dir
# and are never written back to the repo.
#
# Usage:
#   scripts/build-wiki.sh [output-dir]
#
# Defaults to ./wiki-staging/.
#
# Requires: bash, plantuml, find, sed.

set -euo pipefail

OUTDIR="${1:-wiki-staging}"
DOCSROOT="docs"

if [[ ! -d "$DOCSROOT" ]]; then
  echo "ERROR: $DOCSROOT/ not found. Run from repo root." >&2
  exit 1
fi

if ! command -v plantuml >/dev/null 2>&1; then
  echo "ERROR: plantuml not on PATH. Install via apt/brew/sdkman or add to image." >&2
  exit 1
fi

rm -rf "$OUTDIR"
mkdir -p "$OUTDIR"

echo "==> Building wiki staging at $OUTDIR/"

# 1. Top-level pages — copied 1:1 (these are wiki-aware already).
cp "$DOCSROOT/Home.md"     "$OUTDIR/Home.md"
cp "$DOCSROOT/_Sidebar.md" "$OUTDIR/_Sidebar.md"

# 2. Section overview pages — flatten and rename so the wiki URL is clean.
copy_renamed() {
  local src="$1"
  local dst="$2"
  if [[ -f "$src" ]]; then
    cp "$src" "$OUTDIR/$dst"
    echo "    $src -> $dst"
  fi
}

echo "==> Section pages"
copy_renamed "$DOCSROOT/server/README.md"                      "Server.md"
copy_renamed "$DOCSROOT/server/ROADMAP.md"                     "Server-Roadmap.md"
copy_renamed "$DOCSROOT/server/SOLUTION_ARCHITECTURE.md"       "Server-Architecture.md"
copy_renamed "$DOCSROOT/plugins/README.md"                     "Plugins.md"
copy_renamed "$DOCSROOT/plugins/gobservability/README.md"      "Plugin-Gobservability.md"
copy_renamed "$DOCSROOT/plugins/gobservability/ROADMAP.md"     "Plugin-Gobservability-Roadmap.md"
copy_renamed "$DOCSROOT/plugins/gobservability/SOLUTION_ARCHITECTURE.md" \
                                                               "Plugin-Gobservability-Architecture.md"
copy_renamed "$DOCSROOT/gcpc/README.md"                        "GCPC.md"

# 3. Audits — `Audit-<basename>.md` keeps them grouped in the flat namespace.
echo "==> Audits"
if [[ -d "$DOCSROOT/audits" ]]; then
  for f in "$DOCSROOT/audits"/*.md; do
    [[ -f "$f" ]] || continue
    base=$(basename "$f" .md)
    cp "$f" "$OUTDIR/Audit-${base}.md"
    echo "    $f -> Audit-${base}.md"
  done
fi

# 4. Render every .puml to .svg into a tmp area, then bundle by section.
TMPRENDER=$(mktemp -d)
trap 'rm -rf "$TMPRENDER"' EXIT

echo "==> Rendering .puml to .svg"
puml_count=0
while IFS= read -r -d '' puml; do
  # Mirror the source path under TMPRENDER so includes (theme.iuml) resolve.
  reldir=$(dirname "${puml#$DOCSROOT/}")
  mkdir -p "$TMPRENDER/$reldir"
  cp "$puml" "$TMPRENDER/$reldir/"
done < <(find "$DOCSROOT" -name '*.puml' -print0)

# Copy the shared theme so !include ../../../design/theme.iuml resolves.
if [[ -f "$DOCSROOT/design/theme.iuml" ]]; then
  mkdir -p "$TMPRENDER/design"
  cp "$DOCSROOT/design/theme.iuml" "$TMPRENDER/design/theme.iuml"
fi

# Run plantuml on the entire mirror tree at once — much faster than per-file.
plantuml -tsvg -o . "$TMPRENDER/**.puml" >/dev/null 2>&1 || \
  plantuml -tsvg -r "$TMPRENDER" >/dev/null

while IFS= read -r -d '' svg; do
  puml_count=$((puml_count + 1))
done < <(find "$TMPRENDER" -name '*.svg' -print0)
echo "    rendered $puml_count diagram(s)"

# 5. Generate one index page per design subdir, embedding SVGs in source order
#    (alphabetical by filename within the dir — predictable, matches docs/).
generate_design_page() {
  local source_dir="$1"     # e.g. docs/server/design/component
  local out_basename="$2"   # e.g. Server-Components-Diagrams
  local title="$3"          # e.g. Server: Component Diagrams

  local mirror_dir="${source_dir#$DOCSROOT/}"  # server/design/component
  local out_md="$OUTDIR/${out_basename}.md"

  if [[ ! -d "$source_dir" ]]; then
    return 0
  fi

  # Only emit the page if there's at least one .puml in the dir.
  local has_any=0
  for puml in "$source_dir"/*.puml; do
    [[ -f "$puml" ]] && has_any=1 && break
  done
  if [[ $has_any -eq 0 ]]; then
    return 0
  fi

  {
    echo "# ${title}"
    echo
    echo "_Auto-generated from \`${source_dir}/*.puml\` on every push to \`main\`._"
    echo "_Source PlantUML files live in the repo; SVGs below are rendered in the wiki sync workflow._"
    echo
  } > "$out_md"

  for puml in "$source_dir"/*.puml; do
    [[ -f "$puml" ]] || continue
    local name
    name=$(basename "$puml" .puml)
    local svg_src="$TMPRENDER/$mirror_dir/$name.svg"
    if [[ -f "$svg_src" ]]; then
      # Flatten the SVG name into the wiki root so the embed URL is simple.
      local svg_dst_name
      svg_dst_name="diagram-${out_basename}-${name}.svg"
      cp "$svg_src" "$OUTDIR/$svg_dst_name"

      {
        echo "## ${name}"
        echo
        echo "![${name}](${svg_dst_name})"
        echo
      } >> "$out_md"
    else
      {
        echo "## ${name}"
        echo
        echo "_(rendering failed — see CI logs)_"
        echo
      } >> "$out_md"
    fi
  done
  echo "    $source_dir -> ${out_basename}.md"
}

echo "==> Diagram index pages"
generate_design_page "$DOCSROOT/server/design/component" \
                    "Server-Components-Diagrams" \
                    "Server: Component Diagrams"
generate_design_page "$DOCSROOT/server/design/sequence" \
                    "Server-Sequence-Diagrams" \
                    "Server: Sequence Diagrams"
generate_design_page "$DOCSROOT/server/design/state" \
                    "Server-State-Diagrams" \
                    "Server: State Diagrams"

generate_design_page "$DOCSROOT/gcpc/design/component" \
                    "GCPC-Components-Diagrams" \
                    "GCPC: Component Diagrams"
generate_design_page "$DOCSROOT/gcpc/design/sequence" \
                    "GCPC-Sequence-Diagrams" \
                    "GCPC: Sequence Diagrams"
generate_design_page "$DOCSROOT/gcpc/design/state" \
                    "GCPC-State-Diagrams" \
                    "GCPC: State Diagrams"

generate_design_page "$DOCSROOT/plugins/gobservability/design/component" \
                    "Plugin-Gobservability-Components-Diagrams" \
                    "gobservability: Component Diagrams"
generate_design_page "$DOCSROOT/plugins/gobservability/design/sequence" \
                    "Plugin-Gobservability-Sequence-Diagrams" \
                    "gobservability: Sequence Diagrams"

# 6. Rewrite known internal links so cross-page navigation works in the wiki.
#    The body of section pages was authored against the repo path layout
#    (./gobservability/README.md, ../audits/per-shard-arc-summary.md, …) which
#    breaks once everything is flat. We rewrite the most common patterns; rare
#    edge cases will appear as broken-but-clickable links the operator can fix.
echo "==> Rewriting internal links"
declare -A REWRITES=(
  ['](server/README.md)']='](Server)'
  ['](server/README)']='](Server)'
  ['](plugins/README.md)']='](Plugins)'
  ['](plugins/README)']='](Plugins)'
  ['](gcpc/README.md)']='](GCPC)'
  ['](gcpc/README)']='](GCPC)'
  ['](../plugins/README.md)']='](Plugins)'
  ['](../plugins/README)']='](Plugins)'
  ['](../server/README.md)']='](Server)'
  ['](../server/README)']='](Server)'
  ['](../gcpc/README.md)']='](GCPC)'
  ['](../gcpc/README.md#rex-metadata)']='](GCPC#rex-metadata)'
  ['](../audits/per-shard-arc-summary.md)']='](Audit-per-shard-arc-summary)'
  ['](../audits/go-bench-vs-docker-gap.md)']='](Audit-go-bench-vs-docker-gap)'
  ['](../audits/clientctx-cross-goroutine.md)']='](Audit-clientctx-cross-goroutine)'
  ['](audits/per-shard-arc-summary.md)']='](Audit-per-shard-arc-summary)'
  ['](audits/go-bench-vs-docker-gap.md)']='](Audit-go-bench-vs-docker-gap)'
  ['](audits/clientctx-cross-goroutine.md)']='](Audit-clientctx-cross-goroutine)'
  ['](gobservability/README.md)']='](Plugin-Gobservability)'
  ['](gobservability/README)']='](Plugin-Gobservability)'
  ['](SOLUTION_ARCHITECTURE.md)']='](Server-Architecture)'
  ['](SOLUTION_ARCHITECTURE.md#design-diagrams)']='](Server-Architecture#design-diagrams)'
  ['](ROADMAP.md)']='](Server-Roadmap)'
  ['](../server/design/component/)']='](Server-Components-Diagrams)'
  ['](../server/design/sequence/)']='](Server-Sequence-Diagrams)'
  ['](../server/design/state/)']='](Server-State-Diagrams)'
  ['](../gcpc/design/component/)']='](GCPC-Components-Diagrams)'
  ['](../gcpc/design/sequence/)']='](GCPC-Sequence-Diagrams)'
  ['](../gcpc/design/state/)']='](GCPC-State-Diagrams)'
  ['](server/design/component/)']='](Server-Components-Diagrams)'
  ['](server/design/sequence/)']='](Server-Sequence-Diagrams)'
  ['](server/design/state/)']='](Server-State-Diagrams)'
  ['](server/design/)']='](Server-Components-Diagrams)'
  ['](audits)']='](Audit-per-shard-arc-summary)'
)

for md in "$OUTDIR"/*.md; do
  for from in "${!REWRITES[@]}"; do
    to="${REWRITES[$from]}"
    # Use a delimiter that won't appear in either string. ASCII 0x01 is safe.
    sed -i "s$(printf '\1')${from//\//\\/}$(printf '\1')${to//\//\\/}$(printf '\1')g" "$md"
  done
done

echo "==> Done."
echo "    Output: $OUTDIR/"
echo "    File count: $(find "$OUTDIR" -type f | wc -l)"
