#!/usr/bin/env python3
"""Merge two Helm chart repositories into a unified one.

Usage:
    python scripts/merge_helm_repos.py \\
        --repo2-charts /path/to/simplyblock-csi/charts \\
        --base-url https://charts.simplyblock.io

The script merges charts/index.yaml from two repos and copies all chart
.tgz files into a single output directory ready to serve as a Helm repo.
"""

import argparse
import logging
import shutil
import sys
from datetime import datetime, timezone
from pathlib import Path

try:
    import yaml
except ImportError:
    sys.exit("PyYAML is required: pip install pyyaml")

logging.basicConfig(level=logging.INFO, format="%(levelname)s: %(message)s")
log = logging.getLogger(__name__)


def load_index(charts_dir: Path) -> dict:
    index_path = charts_dir / "index.yaml"
    if not index_path.exists():
        raise FileNotFoundError(f"index.yaml not found in {charts_dir}")
    with open(index_path) as f:
        data = yaml.safe_load(f)
    if data.get("apiVersion") != "v1":
        log.warning("%s has apiVersion %s (expected v1)", index_path, data.get("apiVersion"))
    return data


def rewrite_urls(entry: dict, new_base_url: str) -> dict:
    """Reconstruct download URLs under new_base_url using {version}/{filename} layout."""
    version = entry.get("version", "")
    base = new_base_url.rstrip("/")
    new_urls = [
        f"{base}/{version}/{url.split('/')[-1]}"
        for url in entry.get("urls", [])
    ]
    return {**entry, "urls": new_urls}


def merge_entries(entries1: dict, entries2: dict, new_base_url: str) -> dict:
    """Merge two Helm index entries dicts, rewriting URLs to new_base_url."""
    merged: dict[str, list] = {}

    for chart_name, versions in entries1.items():
        merged[chart_name] = [rewrite_urls(v, new_base_url) for v in versions]

    for chart_name, versions in entries2.items():
        rewritten = [rewrite_urls(v, new_base_url) for v in versions]
        if chart_name in merged:
            # Merge by version — avoid duplicates
            existing_versions = {v["version"] for v in merged[chart_name]}
            for v in rewritten:
                if v["version"] not in existing_versions:
                    merged[chart_name].append(v)
                    existing_versions.add(v["version"])
                else:
                    log.warning(
                        "Duplicate chart %s version %s — keeping repo1 entry",
                        chart_name,
                        v["version"],
                    )
        else:
            merged[chart_name] = rewritten

    return merged


def copy_tgz_files(
    charts_dir: Path,
    output_dir: Path,
    dry_run: bool = False,
    force: bool = False,
) -> list[Path]:
    """Copy versioned subdirectory .tgz files from charts_dir into output_dir."""
    copied = []
    for item in sorted(charts_dir.iterdir()):
        if not item.is_dir():
            continue
        # Skip chart source directories (they contain Chart.yaml, not .tgz)
        if (item / "Chart.yaml").exists():
            continue
        for tgz in sorted(item.glob("*.tgz")):
            rel = tgz.relative_to(charts_dir)
            dest = output_dir / rel
            if dest.exists() and not force:
                log.warning("Skipping existing file %s (use --force to overwrite)", dest)
                continue
            log.info("%s -> %s", tgz, dest)
            if not dry_run:
                dest.parent.mkdir(parents=True, exist_ok=True)
                shutil.copy2(tgz, dest)
            copied.append(dest)
    return copied


def build_merged_repo(
    repo1_charts: Path,
    repo2_charts: Path,
    output: Path,
    base_url: str,
    dry_run: bool = False,
    force: bool = False,
) -> None:
    log.info("Loading index from repo1: %s", repo1_charts)
    index1 = load_index(repo1_charts)
    log.info("Loading index from repo2: %s", repo2_charts)
    index2 = load_index(repo2_charts)

    entries1 = index1.get("entries", {})
    entries2 = index2.get("entries", {})
    log.info(
        "repo1 charts: %s | repo2 charts: %s",
        ", ".join(sorted(entries1)) or "(none)",
        ", ".join(sorted(entries2)) or "(none)",
    )

    merged_entries = merge_entries(entries1, entries2, base_url)

    merged_index = {
        "apiVersion": "v1",
        "entries": merged_entries,
        "generated": datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%S.%f+00:00"),
    }

    if not dry_run:
        output.mkdir(parents=True, exist_ok=True)

    # Copy .tgz files from both repos
    copied1 = copy_tgz_files(repo1_charts, output, dry_run=dry_run, force=force)
    copied2 = copy_tgz_files(repo2_charts, output, dry_run=dry_run, force=force)

    # Write merged index.yaml
    index_out = output / "index.yaml"
    log.info("Writing merged index.yaml -> %s", index_out)
    if not dry_run:
        with open(index_out, "w") as f:
            yaml.dump(merged_index, f, default_flow_style=False, allow_unicode=True, sort_keys=True)

    # Summary
    total_charts = sum(len(v) for v in merged_entries.values())
    print(
        f"\nMerge complete:\n"
        f"  Charts: {', '.join(sorted(merged_entries))}\n"
        f"  Total versions: {total_charts}\n"
        f"  .tgz files copied: {len(copied1)} (repo1) + {len(copied2)} (repo2)\n"
        f"  Output: {output}\n"
        f"  Base URL: {base_url}"
    )
    if dry_run:
        print("  (dry-run: no files were written)")


def main() -> None:
    parser = argparse.ArgumentParser(
        description="Merge two Helm chart repositories into a unified one.",
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog=__doc__,
    )
    parser.add_argument(
        "--repo1-charts",
        default="./charts",
        type=Path,
        metavar="DIR",
        help="Path to first repo's charts directory (default: ./charts)",
    )
    parser.add_argument(
        "--repo2-charts",
        required=True,
        type=Path,
        metavar="DIR",
        help="Path to second repo's charts directory",
    )
    parser.add_argument(
        "--output",
        default="./merged-charts",
        type=Path,
        metavar="DIR",
        help="Output directory for merged repo (default: ./merged-charts)",
    )
    parser.add_argument(
        "--base-url",
        default="https://charts.simplyblock.io",
        metavar="URL",
        help="Base URL for the unified Helm repo (default: https://charts.simplyblock.io)",
    )
    parser.add_argument(
        "--force",
        action="store_true",
        help="Overwrite existing .tgz files in the output directory",
    )
    parser.add_argument(
        "--dry-run",
        action="store_true",
        help="Print what would be done without writing any files",
    )
    args = parser.parse_args()

    build_merged_repo(
        repo1_charts=args.repo1_charts,
        repo2_charts=args.repo2_charts,
        output=args.output,
        base_url=args.base_url,
        dry_run=args.dry_run,
        force=args.force,
    )


if __name__ == "__main__":
    main()
