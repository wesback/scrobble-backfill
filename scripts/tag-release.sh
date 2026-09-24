#!/usr/bin/env bash

set -u

fail() {
	printf 'tag-release: %s\n' "$*" >&2
	exit 1
}

if [[ $# -ne 1 ]]; then
	fail "usage: $0 <vX.Y.Z>"
fi

tag=$1
if [[ ! $tag =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
	fail "invalid version '$tag'; expected vX.Y.Z"
fi

status=$(git status --porcelain) || fail "could not read git working-tree status"
if [[ -n $status ]]; then
	fail "working tree is not clean"
fi

branch=$(git branch --show-current) || fail "could not determine the current branch"
if [[ $branch != main ]]; then
	fail "current branch must be main (found '${branch:-detached HEAD}')"
fi

git fetch origin main:refs/remotes/origin/main || fail "could not fetch origin/main"

head=$(git rev-parse HEAD) || fail "could not resolve HEAD"
origin_main=$(git rev-parse origin/main) || fail "could not resolve origin/main"
if [[ $head != "$origin_main" ]]; then
	fail "HEAD is not up to date with origin/main"
fi

if git rev-parse --verify --quiet "refs/tags/$tag" >/dev/null; then
	fail "tag '$tag' already exists locally"
fi

remote_tag=$(git ls-remote --tags origin "refs/tags/$tag") ||
	fail "could not check for tag '$tag' on origin"
if [[ -n $remote_tag ]]; then
	fail "tag '$tag' already exists on origin"
fi

if ! go test ./...; then
	fail "go test ./... failed; tag '$tag' was not created or pushed"
fi

git tag -a "$tag" -m "$tag" || fail "could not create tag '$tag'"
if ! git push origin "$tag"; then
	if git tag -d "$tag"; then
		fail "could not push tag '$tag' to origin; removed the local tag"
	fi
	fail "could not push tag '$tag' to origin, and could not remove the local tag"
fi

printf 'tag-release: pushed %s\n' "$tag"
