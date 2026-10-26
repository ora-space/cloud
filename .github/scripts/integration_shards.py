"""Partition Go's discovered integration tests without maintaining a test-name list."""

import argparse
from collections import Counter
import json
from pathlib import Path
import re
import sys
import zlib


SHARDS = 4
# Go also accepts the bare identifiers Test, Example and Fuzz.
TEST_NAME = re.compile(r"(?:Test|Example|Fuzz)\w*\Z")


def require(condition, message):
    if not condition:
        raise SystemExit(message)


def partition(names):
    # CRC32 is only a stable name-to-job assignment, not a file integrity check.
    groups = [[] for _ in range(SHARDS)]
    for name in sorted(names):
        groups[zlib.crc32(name.encode("utf-8")) % SHARDS].append(name)
    require(Counter(n for group in groups for n in group) == Counter(names),
            "shard union differs from go test -list (missing or duplicated tests)")
    require(all(groups), "empty shard would select no tests")
    return groups


def plan(list_path, shard, output):
    # Benchmarks are not run by the original go test command. Examples and fuzz seeds are.
    names = [line for line in list_path.read_text().splitlines() if TEST_NAME.fullmatch(line)]
    require(names and len(names) == len(set(names)), "empty or duplicate go test -list names")
    groups = partition(names)
    selected = groups[shard]
    output.write_text(json.dumps({"shard": shard, "all": sorted(names), "selected": selected}, indent=2) + "\n")
    print(f"SHARD_PLAN shard={shard} selected={len(selected)} total={len(names)} "
          f"counts={','.join(str(len(g)) for g in groups)}", file=sys.stderr)
    print("pattern=^(" + "|".join(re.escape(name) for name in selected) + ")$")


def record(manifest, log):
    data = json.loads(manifest.read_text())
    starts = []
    finished = []
    for line in log.read_text().splitlines():
        start = re.fullmatch(r"=== RUN\s+(\S+)", line)
        end = re.fullmatch(r"--- (?P<status>PASS|FAIL|SKIP): (?P<name>\S+) \([0-9.]+s\)", line)
        if start and "/" not in start[1]:
            starts.append(start[1])
        if end and "/" not in end["name"]:
            require(end["status"] != "FAIL", f"failed top-level test: {end['name']}")
            finished.append(end["name"])
    expected = Counter(data["selected"])
    require(Counter(starts) == expected, "actual top-level starts differ from planned shard")
    require(Counter(finished) == expected, "actual top-level results differ from planned shard")
    data["executed"] = sorted(finished)
    manifest.write_text(json.dumps(data, indent=2) + "\n")
    print(f"SHARD_EXECUTED shard={data['shard']} count={len(finished)} total={len(data['all'])}")


def verify(directory):
    paths = sorted(directory.glob("coverage-*/shard.json"))
    require(len(paths) == SHARDS, f"expected {SHARDS} shard manifests, found {len(paths)}")
    data = [json.loads(path.read_text()) for path in paths]
    require(sorted(d["shard"] for d in data) == list(range(SHARDS)), "missing or duplicate shard IDs")
    names = data[0]["all"]
    groups = partition(names)
    executed = []
    for entry in data:
        require(entry["all"] == names, "shards discovered different test lists")
        require(entry["selected"] == groups[entry["shard"]], "shard assignment differs from rule")
        require(entry.get("executed") == entry["selected"], "shard did not execute every selected test once")
        executed.extend(entry["executed"])
    require(Counter(executed) == Counter(names), "executed union differs from go test -list")
    print(f"SHARD_UNION discovered={len(names)} executed={len(executed)} "
          f"unique={len(set(executed))} counts={','.join(str(len(g)) for g in groups)}")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    p = commands.add_parser("plan")
    p.add_argument("list_path", type=Path)
    p.add_argument("shard", type=int, choices=range(SHARDS))
    p.add_argument("output", type=Path)
    p = commands.add_parser("record")
    p.add_argument("manifest", type=Path)
    p.add_argument("log", type=Path)
    p = commands.add_parser("verify")
    p.add_argument("directory", type=Path)
    args = vars(parser.parse_args())
    command = args.pop("command")
    {"plan": plan, "record": record, "verify": verify}[command](**args)


if __name__ == "__main__":
    main()
