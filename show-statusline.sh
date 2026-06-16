#!/bin/bash
# show-statusline.sh — Claude Code statusline hook
# 3-line layout: location | cost | limits/context
# set -euo pipefail は意図的に省略（部分失敗でも残りを表示する設計）

input=$(cat)

# === stdin JSONから一括抽出（location） ===
{
  read -r session_id
  read -r project_dir
  read -r current_dir
  read -r context_remaining
} < <(echo "$input" | jq -r '
  (.session_id // ""),
  (.workspace.project_dir // ""),
  (.workspace.current_dir // .cwd // ""),
  (.context_window.remaining_percentage // "")
' 2>/dev/null)

# パス表示（~/project > subdir）
display_path=""
if [ -n "$project_dir" ]; then
  project_short="${project_dir/#$HOME/~}"
  if [ -n "$current_dir" ] && [ "$current_dir" != "$project_dir" ] && \
     [[ "$current_dir" == "$project_dir"/* ]]; then
    display_path="${project_short} > ${current_dir##*/}"
  else
    display_path="${project_short}"
  fi
fi

# Git ブランチ + HEAD との diff（uncommitted: staged + unstaged）
git_branch=""
git_diff=""
pr_display=""
target_dir="${current_dir:-$project_dir}"

format_diff_segment() {
  local stat="$1"
  local ins del
  ins=$(echo "$stat" | grep -oE '[0-9]+ insertion' | grep -oE '[0-9]+' | head -1)
  del=$(echo "$stat" | grep -oE '[0-9]+ deletion' | grep -oE '[0-9]+' | head -1)
  [ -z "$ins" ] && ins=0
  [ -z "$del" ] && del=0
  if [ "$ins" -gt 0 ] || [ "$del" -gt 0 ]; then
    printf '\033[32m+%s\033[0m \033[31m-%s\033[0m' "$ins" "$del"
  fi
}

if [ -n "$target_dir" ] && [ -d "$target_dir" ]; then
  git_branch=$(git -C "$target_dir" branch --show-current 2>/dev/null)
  if [ -n "$git_branch" ]; then
    diff_stat=$(git -C "$target_dir" diff HEAD --shortstat 2>/dev/null)
    [ -n "$diff_stat" ] && git_diff=$(format_diff_segment "$diff_stat")

    # PR 情報（gh CLI + 60秒キャッシュ）
    if command -v gh >/dev/null 2>&1 \
       && [ "$git_branch" != "main" ] && [ "$git_branch" != "master" ]; then
      cache_key=$(echo "${target_dir}:${git_branch}" | shasum 2>/dev/null | awk '{print $1}')
      pr_cache_file="/tmp/cc-pr-cache-$(id -u 2>/dev/null)-${cache_key}.json"
      pr_json=""
      if [ -f "$pr_cache_file" ]; then
        mtime=$(stat -f %m "$pr_cache_file" 2>/dev/null || stat -c %Y "$pr_cache_file" 2>/dev/null)
        now=$(date +%s)
        if [ -n "$mtime" ] && [ $((now - mtime)) -lt 60 ]; then
          pr_json=$(cat "$pr_cache_file" 2>/dev/null)
        fi
      fi
      if [ -z "$pr_json" ]; then
        pr_json=$(cd "$target_dir" && gh pr view --json number,baseRefName 2>/dev/null)
        [ -z "$pr_json" ] && pr_json="{}"
        echo "$pr_json" > "$pr_cache_file" 2>/dev/null
      fi
      pr_number=$(echo "$pr_json" | jq -r '.number // empty' 2>/dev/null)
      base_ref=$(echo "$pr_json" | jq -r '.baseRefName // empty' 2>/dev/null)
      if [ -n "$pr_number" ]; then
        pr_display="#${pr_number}"
        if [ -n "$base_ref" ]; then
          pr_stat=$(git -C "$target_dir" diff "origin/${base_ref}...HEAD" --shortstat 2>/dev/null)
          pr_seg=$(format_diff_segment "$pr_stat")
          [ -n "$pr_seg" ] && pr_display="${pr_display} ${pr_seg}"
        fi
      fi
    fi
  fi
fi

# === コスト情報 ===
{
  read -r model_name
  read -r session_cost
  read -r total_duration_ms
  read -r api_duration_ms
} < <(echo "$input" | jq -r '
  (.model.display_name // ""),
  (.cost.total_cost_usd // ""),
  (.cost.total_duration_ms // ""),
  (.cost.total_api_duration_ms // "")
' 2>/dev/null)

format_duration() {
  local ms="$1"
  [ -z "$ms" ] || [ "$ms" = "null" ] && return
  local s=$((ms / 1000))
  local d=$((s / 86400))
  local h=$(((s % 86400) / 3600))
  local m=$(((s % 3600) / 60))
  if [ "$d" -gt 0 ]; then echo "${d}d ${h}h"
  elif [ "$h" -gt 0 ]; then echo "${h}h ${m}m"
  else echo "${m}m"; fi
}

session_cost_display=""
[ -n "$session_cost" ] && [ "$session_cost" != "null" ] && \
  session_cost_display=$(printf '$%.2f' "$session_cost")

# 日次コスト・バーンレート（Go binary; build with `go build -o calc-daily-cost ./calc-daily-cost.go`）
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
HELPER_BIN="$SCRIPT_DIR/calc-daily-cost"
daily_cost_json=""
[ -x "$HELPER_BIN" ] && daily_cost_json=$("$HELPER_BIN" daily-cost 2>/dev/null)

daily_cost_display=""
burn_rate_display=""
if [ -n "$daily_cost_json" ]; then
  dc=$(echo "$daily_cost_json" | jq -r '.daily_cost // ""' 2>/dev/null)
  br=$(echo "$daily_cost_json" | jq -r '.burn_rate_cost_per_hour // ""' 2>/dev/null)
  [ -n "$dc" ] && [ "$dc" != "null" ] && daily_cost_display=$(printf '$%.2f' "$dc")
  [ -n "$br" ] && [ "$br" != "null" ] && burn_rate_display="$(printf '$%.2f' "$br")/hr"
fi

# === rate_limits（5h / 7d）プログレスバー（Go binary subcommand） ===
usage_display=""
[ -x "$HELPER_BIN" ] && usage_display=$(echo "$input" | "$HELPER_BIN" rate-limits 2>/dev/null)

# === コンテキスト消費率 ===
context_display=""
if [ -n "$context_remaining" ] && [ "$context_remaining" != "null" ]; then
  ctx_remain="${context_remaining%.*}"
  [ -z "$ctx_remain" ] && ctx_remain=0
  [ "$ctx_remain" -lt 0 ] 2>/dev/null && ctx_remain=0
  [ "$ctx_remain" -gt 100 ] 2>/dev/null && ctx_remain=100
  ctx_used=$((100 - ctx_remain))
  filled=$((ctx_used / 10))
  empty=$((10 - filled))
  bar=""
  for ((i=0; i<filled; i++)); do bar+="█"; done
  for ((i=0; i<empty; i++)); do bar+="░"; done
  if [ "$ctx_used" -lt 40 ]; then color=$'\033[32m'
  elif [ "$ctx_used" -lt 70 ]; then color=$'\033[33m'
  else color=$'\033[31m'; fi
  reset_color=$'\033[0m'
  context_display="ctx ${color}${bar}${reset_color} ${ctx_used}%"
fi

# === 3行出力 ===
line1=""
[ -n "$session_id" ] && line1="📍 ${session_id}"
[ -n "$display_path" ] && line1="${line1:+$line1 | }📁 ${display_path}"
if [ -n "$git_branch" ]; then
  branch_seg="🌿 ${git_branch}"
  [ -n "$git_diff" ] && branch_seg="${branch_seg} ${git_diff}"
  [ -n "$pr_display" ] && branch_seg="${branch_seg} | ${pr_display}"
  line1="${line1:+$line1 | }${branch_seg}"
fi

cost_line=""
if [ -n "$model_name" ]; then
  cost_line="🤖 ${model_name}"
  [ -n "$context_display" ] && cost_line="$cost_line | ${context_display}"
  [ -n "$usage_display" ] && cost_line="$cost_line | ${usage_display}"
  [ -n "$daily_cost_display" ] && cost_line="$cost_line | ${daily_cost_display} today"
fi

[ -n "$line1" ] && echo "$line1"
[ -n "$cost_line" ] && echo "$cost_line"
