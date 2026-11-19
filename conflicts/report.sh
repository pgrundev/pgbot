#!/usr/bin/env bash
# Reports database conflicts on GitHub: this PR's comment and check run, and,
# with UPDATE_OTHERS, the comment and check run of every other open PR whose
# report this PR changes (a new conflict with it, or one that just cleared).
#
# A check run is what GitHub shows live in the PR's merge box ("Database
# conflicts — 1 conflict with #64"), with no page reload; the comment carries
# the detail.
#
# Env: REPO, DIR (holds mine/changeset.json, others/, prs.json), LABEL,
# PR_NUMBER, HEAD_SHA, COMMENT, CHECK_RUN, UPDATE_OTHERS, CHECK_NAME,
# CONCLUSION_ON_CONFLICT. Uses gh (GH_TOKEN), jq, pgbot.
set -euo pipefail

marker='<!-- pgbot-schema-conflicts -->'
CHECK_NAME="${CHECK_NAME:-Database conflicts}"
CONCLUSION_ON_CONFLICT="${CONCLUSION_ON_CONFLICT:-failure}"

# report <focus label> <out prefix>: json + markdown focused on one PR
report() {
  pgbot schema conflicts "$DIR/mine/changeset.json" "$DIR/others" --focus "$1" --format json > "$2.json"
  pgbot schema conflicts "$DIR/mine/changeset.json" "$DIR/others" --focus "$1" --format markdown > "$2.md"
}

items() { jq '.summary.conflicts + .summary.overlaps' "$1.json"; }

# comment_id <pr>: the id of our earlier comment on that PR, "" if none
comment_id() {
  gh api --paginate "repos/$REPO/issues/$1/comments" \
    --jq ".[] | select(.body | startswith(\"$marker\")) | .id" | head -n 1
}

# upsert_comment <pr> <prefix>: edit our comment in place; post a new one
# only when there is something to say
upsert_comment() {
  local pr="$1" p="$2" id
  id=$(comment_id "$pr")
  if [ -n "$id" ]; then
    gh api -X PATCH "repos/$REPO/issues/comments/$id" -F "body=@$p.md" --jq .html_url
  elif [ "$(items "$p")" -gt 0 ]; then
    gh api -X POST "repos/$REPO/issues/$pr/comments" -F "body=@$p.md" --jq .html_url
  else
    echo "#$pr: nothing to report and no earlier comment: staying quiet"
  fi
}

# check_run <sha> <prefix>
check_run() {
  local sha="$1" p="$2" conclusion title
  conclusion=$(jq -r --arg c "$CONCLUSION_ON_CONFLICT" \
    'if .summary.conflicts > 0 then $c elif .summary.overlaps > 0 then "neutral" else "success" end' "$p.json")
  title=$(jq -r '
    ([.items[].b] | unique | join(", ")) as $who
    | if .summary.conflicts + .summary.overlaps == 0 then
        "No conflicts with \(.changesets | length - 1) other open PR(s)"
      else
        [ (if .summary.conflicts > 0 then "\(.summary.conflicts) conflict(s)" else empty end),
          (if .summary.overlaps > 0 then "\(.summary.overlaps) overlap(s)" else empty end) ]
        | join(", ") + " with " + $who
      end' "$p.json")
  jq -n --arg name "$CHECK_NAME" --arg sha "$sha" --arg c "$conclusion" --arg t "$title" --rawfile md "$p.md" \
    '{name: $name, head_sha: $sha, status: "completed", conclusion: $c,
      output: {title: $t, summary: ($md | .[0:65000])}}' \
    | gh api -X POST "repos/$REPO/check-runs" --input - --jq '"\(.name): \(.output.title) \(.html_url)"'
}

failed=0
try() { "$@" || { echo "::warning::could not update GitHub: $*"; failed=1; }; }

# --- this PR
report "$LABEL" "$DIR/report-mine"
[ "$COMMENT" = true ] && try upsert_comment "$PR_NUMBER" "$DIR/report-mine"
[ "$CHECK_RUN" = true ] && try check_run "$HEAD_SHA" "$DIR/report-mine"

# --- the other PRs this one collides with, or collided with before
if [ "$UPDATE_OTHERS" = true ] && [ -s "$DIR/prs.json" ]; then
  for f in "$DIR"/others/*/changeset.json; do
    [ -e "$f" ] || continue
    other=$(jq -r .label "$f")
    n="${other#\#}"
    case "$n" in '' | *[!0-9]*) continue ;; esac # labels other than #<number> can't be addressed
    sha=$(jq -r --argjson n "$n" '.[] | select(.number == $n) | .headRefOid' "$DIR/prs.json")
    [ -n "$sha" ] || continue
    report "$other" "$DIR/report-$n"
    with_me=$(jq --arg me "$LABEL" '[.items[] | select(.b == $me)] | length' "$DIR/report-$n.json")
    id=$(comment_id "$n" || true)
    mentioned=0
    if [ -n "$id" ]; then
      mentioned=$(gh api "repos/$REPO/issues/comments/$id" --jq .body \
        | jq -Rs --arg me "$LABEL" 'if test("(^|[^0-9A-Za-z])" + $me + "([^0-9]|$)") then 1 else 0 end')
    fi
    if [ "$with_me" -eq 0 ] && [ "$mentioned" -eq 0 ]; then
      continue # this PR does not change what #n's report says
    fi
    echo "#$n: refreshing its report ($with_me item(s) with $LABEL)"
    [ "$COMMENT" = true ] && try upsert_comment "$n" "$DIR/report-$n"
    [ "$CHECK_RUN" = true ] && try check_run "$sha" "$DIR/report-$n"
  done
fi

exit "$failed"
