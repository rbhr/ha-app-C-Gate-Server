#!/usr/bin/env python3
"""Read-only release preflight. Publication remains in the GitHub workflow."""
import base64
import json
import os
from pathlib import Path
import re
from urllib.parse import urlencode
from urllib.request import Request, urlopen

SEMVER = re.compile(r"^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$")


def version_tuple(value):
    if not SEMVER.fullmatch(value):
        raise ValueError("release version must be a stable major.minor.patch")
    return tuple(map(int, value.split(".")))


def validate_release(tag, version, prerelease, existing):
    candidate = version_tuple(version)
    if tag != "v" + version or prerelease != "false":
        raise ValueError("release tag must match config.yaml and must not be a prerelease")
    published = [version_tuple(v) for v in existing if SEMVER.fullmatch(v)]
    if published and candidate <= max(published):
        raise ValueError("release must be newer than every published version; refusing overwrite or latest rollback")


def get_json(url, headers):
    with urlopen(Request(url, headers=headers), timeout=30) as response:
        return json.load(response), response.headers


def registry_tags(repository):
    auth = base64.b64encode((os.environ["GITHUB_ACTOR"] + ":" + os.environ["REGISTRY_TOKEN"]).encode()).decode()
    query = urlencode({"service": "ghcr.io", "scope": "repository:" + repository + ":pull"})
    token, _ = get_json("https://ghcr.io/token?" + query, {"Authorization": "Basic " + auth})
    headers = {"Authorization": "Bearer " + token["token"]}
    url = "https://ghcr.io/v2/" + repository + "/tags/list?n=1000"
    tags = []
    while url:
        data, response_headers = get_json(url, headers)
        tags.extend(data.get("tags") or [])
        link = response_headers.get("Link", "")
        url = ""
        if link:
            match = re.fullmatch(r'<([^>]+)>;\s*rel="?next"?', link)
            if not match:
                raise ValueError("unexpected registry pagination response")
            url = match.group(1)
            if url.startswith("/"):
                url = "https://ghcr.io" + url
            if not url.startswith("https://ghcr.io/v2/" + repository + "/"):
                raise ValueError("registry pagination changed origin or repository")
    return tags


def main():
    config = Path("cgate-server/config.yaml").read_text()
    version = re.search(r"^version:\s*(\S+)\s*$", config, re.M).group(1).strip("\"'")
    tag = os.environ["RELEASE_TAG"]
    prerelease = os.environ["RELEASE_PRERELEASE"]
    owner = os.environ["GITHUB_REPOSITORY"].split("/")[0].lower()
    validate_release(tag, version, prerelease, [])
    for image in ("amd64-cgate-server", "aarch64-cgate-server", "cgate-server"):
        validate_release(tag, version, prerelease, registry_tags(owner + "/" + image))
    print("Release", version, "matches the manifest and is newer than all published image tags.")


if __name__ == "__main__":
    main()
