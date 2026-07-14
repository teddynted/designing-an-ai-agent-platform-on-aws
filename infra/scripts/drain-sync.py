#!/usr/bin/env python3
"""Keep the drain agent in the compute template identical to the canonical script.

The Spot drain agent (Milestone 3) now has to exist in two places:

  * scripts/ami/spot-drain.sh — the source of truth, and what the custom AMI bakes.
  * cloudformation/03-compute.yaml — embedded in the traditional UserData, for an
    instance launched WITHOUT a custom AMI, which has to write the agent out at
    boot because nothing put it there.

The alternative to duplicating it was to fetch the script from S3 at boot, which
trades a drift risk for a boot-time network dependency on the very code path
whose job is to survive things going wrong. Duplication plus a check is the
better trade — but only if the check actually runs.

    drain-sync.py --check    # fail if the two have drifted (CI, and `make lint`)
    drain-sync.py --emit     # print the block to paste into the template

The only transformation between them is CloudFormation's: inside Fn::Sub, `${X}`
is a template variable, so a shell `${X}` must be escaped as `${!X}`.
"""

import argparse
import pathlib
import re
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
SCRIPT = ROOT / "scripts" / "ami" / "spot-drain.sh"
UNIT = ROOT / "scripts" / "ami" / "spot-drain.service"
TEMPLATE = ROOT / "cloudformation" / "03-compute.yaml"

INDENT = " " * 16
BEGIN, END = "# --- BEGIN spot-drain.sh ---", "# --- END spot-drain.sh ---"


def escape_for_sub(text: str) -> str:
    """Escape shell ${VAR} so Fn::Sub emits it literally instead of substituting it."""
    return re.sub(r"\$\{(?!!)", "${!", text)


def render() -> str:
    """The exact block the template must contain, between the BEGIN/END markers."""
    script = escape_for_sub(SCRIPT.read_text().rstrip("\n"))
    unit = escape_for_sub(UNIT.read_text().rstrip("\n"))

    lines = [f"{INDENT}{BEGIN}", f"{INDENT}cat > /usr/local/bin/spot-drain <<'DRAIN'"]
    lines += [f"{INDENT}{line}".rstrip() for line in script.split("\n")]
    lines += [f"{INDENT}DRAIN", f"{INDENT}{END}", f"{INDENT}chmod 0755 /usr/local/bin/spot-drain", ""]
    lines += [f"{INDENT}cat > /etc/systemd/system/spot-drain.service <<'UNIT'"]
    lines += [f"{INDENT}{line}".rstrip() for line in unit.split("\n")]
    lines += [f"{INDENT}UNIT"]
    return "\n".join(lines)


def embedded() -> str:
    """What the template currently contains, from BEGIN to the end of the unit heredoc."""
    text = TEMPLATE.read_text()
    start = text.find(f"{INDENT}{BEGIN}")
    if start == -1:
        sys.exit(f"error: {TEMPLATE.name} has no '{BEGIN}' marker — the check cannot verify anything")
    end = text.find(f"{INDENT}UNIT\n", text.find(f"{INDENT}{END}"))
    if end == -1:
        sys.exit(f"error: {TEMPLATE.name} has no closing UNIT heredoc after '{END}'")
    return text[start : end + len(f"{INDENT}UNIT")]


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--check", action="store_true", help="fail if the copies have drifted")
    parser.add_argument("--emit", action="store_true", help="print the block for the template")
    parser.add_argument("--write", action="store_true", help="regenerate the block in the template")
    args = parser.parse_args()

    if args.emit:
        print(render())
        return 0

    if args.write:
        current = embedded()
        if current == render():
            print(f"{TEMPLATE.name} is already in sync")
            return 0
        TEMPLATE.write_text(TEMPLATE.read_text().replace(current, render()))
        print(f"regenerated the drain agent in {TEMPLATE.name} from {SCRIPT.name}")
        return 0

    if args.check:
        if embedded() == render():
            print("drain agent in 03-compute.yaml matches scripts/ami/spot-drain.sh")
            return 0
        print(
            f"error: the drain agent embedded in {TEMPLATE.name} has drifted from "
            f"{SCRIPT.relative_to(ROOT)}.\n"
            "       They must be identical: the AMI bakes one, and an instance launched\n"
            "       without a custom AMI writes out the other. Regenerate with:\n"
            "           make -C infra drain-sync",
            file=sys.stderr,
        )
        return 1

    parser.print_help()
    return 1


if __name__ == "__main__":
    sys.exit(main())
