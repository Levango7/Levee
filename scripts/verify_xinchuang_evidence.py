#!/usr/bin/env python3
"""One-command recheck of the xinchuang (信创) drill evidence on this lab machine.

Why this exists: the drill produced four ISO images and four guest VMs on a
single Windows 11 host (QEMU 11.1.3 + WHPX).  Every "已验证" cell in
docs/platform-support-matrix.md rests on (a) that ISO, and (b) that guest.
Docs pin the sha256 of each ISO, but a doc cannot stop someone re-downloading
or moving the files — this script re-proves the cheap half: the four ISOs still
hash to the pinned values, and the four guests still answer on the SSH ports
the drills used.  It does NOT re-run the drills themselves (that needs SSH into
each guest plus the drill scripts — see docs/xinchuang-vm-drill.md §3).

Exit codes:
  0  every ISO found, every sha256 matches (live VMs are probed and reported;
     a stopped guest is SKIP, not FAIL — guests are expected to be off at rest)
  1  an ISO is missing or a sha256 mismatches
  2  argument/environment error (bad --iso-dir)

The ISOs are large, so hashing takes minutes on this machine.  Pass --skip-hash
to only probe the guests, or --skip-vm to only hash the ISOs.
"""

import argparse
import hashlib
import os
import socket
import sys

# Pinned in docs/xinchuang-acceptance-2026-11.md §2.  Change these only together
# with the docs — the whole point is that the doc and the evidence cannot drift.
ISOS = [
    ("Kylin-Server-V10-SP3-2403-X86_64.iso",
     "039681842fc354789ed4471f9debdf0204a4e64607db59a9d8fecb37c4ed77a3"),
    ("Kylin-Server-V11-2503-Release-General-20250715-X86_64.iso",
     "05569c9ac23ded1f6768ccedc60a344e0c24cec5075fd54670e3e7a7a4428d7e"),
    ("uniontechos-server-20-1050e-amd64.iso",
     "614ae5a7cfaf4f7750a5e5b09a6bcc7d78ac99fbffd78a3eb593229d4a88c185"),
    ("uos-server-25-2510-amd64.iso",
     "825ec55f76166584b2bd923d41f9938e18ed380272e7315a57b587e2d89b17b2"),
]

VMS = [
    ("kylin-v10-sp3", 22222),
    ("kylin-v11", 22223),
    ("uos-server-20-1050e", 22224),
    ("uos-server-25-2510", 22225),
]

CHUNK = 16 * 1024 * 1024


def sha256(path: str) -> str:
    h = hashlib.sha256()
    with open(path, "rb") as f:
        while True:
            block = f.read(CHUNK)
            if not block:
                break
            h.update(block)
    return h.hexdigest()


def probe(port: int, timeout: float = 2.0) -> bool:
    try:
        with socket.create_connection(("127.0.0.1", port), timeout=timeout):
            return True
    except OSError:
        return False


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--iso-dir",
        default=os.environ.get("XINCHUANG_ISO_DIR") or r"G:\levee-vm",
        help="directory holding the four ISOs (default: $XINCHUANG_ISO_DIR or G:\\levee-vm)",
    )
    parser.add_argument("--skip-hash", action="store_true", help="skip ISO hashing (probe VMs only)")
    parser.add_argument("--skip-vm", action="store_true", help="skip VM reachability probes")
    args = parser.parse_args(argv)

    if not os.path.isdir(args.iso_dir):
        print(f"FAIL: iso-dir does not exist: {args.iso_dir}")
        return 2

    hard_fail = 0

    print(f"== ISO sha256 ({args.iso_dir}) ==")
    if args.skip_hash:
        print("skipped (--skip-hash)")
    else:
        for name, expected in ISOS:
            path = os.path.join(args.iso_dir, name)
            if not os.path.isfile(path):
                print(f"MISSING  {name}")
                hard_fail += 1
                continue
            got = sha256(path)
            ok = got.lower() == expected.lower()
            print(f"{'OK     ' if ok else 'MISMATCH'}  {name}")
            if not ok:
                print(f"         expected {expected}\n         got      {got}")
                hard_fail += 1

    print()
    print("== guest reachability (127.0.0.1) ==")
    if args.skip_vm:
        print("skipped (--skip-vm)")
    else:
        for name, port in VMS:
            up = probe(port)
            print(f"{'UP     ' if up else 'DOWN   '}  {name} (ssh :{port})"
                  + ("" if up else "  [guest stopped — not a failure]"))

    print()
    if hard_fail:
        print(f"RESULT: FAIL ({hard_fail} hard failure(s))")
        return 1
    print("RESULT: PASS (or all checks legitimately skipped)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
