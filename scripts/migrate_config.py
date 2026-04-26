#!/usr/bin/env -S uv run --script
# /// script
# requires-python = ">=3.10"
# dependencies = ["ruamel.yaml"]
# ///
"""Migrate MoonBridge config.yml from the old format to the new routes-based format.

Old format (per-provider):
  provider:
    providers:
      deepseek:
        models:
          moonbridge:            # alias as key
            name: deepseek-v4-pro  # upstream model name
            context_window: 1000000
            pricing:
              input_price: 2

New format:
  provider:
    providers:
      deepseek:
        models:
          deepseek-v4-pro:       # upstream model name as key, no "name" field
            context_window: 1000000
            pricing:
              input_price: 2
    routes:
      moonbridge: "deepseek/deepseek-v4-pro"

Usage:
  python3 script/migrate_config.py                     # reads config.yml, writes config.yml
  python3 script/migrate_config.py old.yml             # reads old.yml, writes old.yml
  python3 script/migrate_config.py old.yml new.yml     # reads old.yml, writes new.yml
  python3 script/migrate_config.py --dry-run old.yml   # preview without writing
"""

from __future__ import annotations

import argparse
import copy
import sys
from pathlib import Path

from ruamel.yaml import YAML


def needs_migration(provider_block: dict) -> bool:
    """Return True if any provider model entry still uses the old 'name' field."""
    providers = provider_block.get("providers")
    if not providers:
        return False
    for pdef in providers.values():
        models = pdef.get("models")
        if not models:
            continue
        for mdef in models.values():
            if isinstance(mdef, dict) and "name" in mdef:
                return True
    return False


def migrate(data: dict) -> dict:
    """Transform the config dict in-place from old to new format."""
    provider_block = data.get("provider")
    if not provider_block:
        return data

    if not needs_migration(provider_block):
        print("Config already uses the new format (no 'name' fields found). Nothing to do.")
        return data

    providers = provider_block.get("providers", {})
    routes: dict[str, str] = {}

    for provider_key, pdef in providers.items():
        old_models = pdef.get("models")
        if not old_models:
            continue

        new_models: dict = {}
        for alias, mdef in old_models.items():
            if not isinstance(mdef, dict):
                # Bare value or empty — treat alias as upstream name.
                new_models[alias] = mdef
                routes[alias] = f"{provider_key}/{alias}"
                continue

            upstream_name = mdef.pop("name", None)
            if not upstream_name:
                # No "name" field — alias IS the upstream name (already new format).
                new_models[alias] = mdef
                routes[alias] = f"{provider_key}/{alias}"
                continue

            # Migrate: alias -> upstream_name, strip "name" field.
            # If multiple aliases point to the same upstream model, merge metadata
            # (last-write-wins for simplicity).
            cleaned = copy.deepcopy(mdef)
            if upstream_name in new_models:
                # Merge: keep existing metadata, overlay new.
                existing = new_models[upstream_name]
                if isinstance(existing, dict):
                    existing.update({k: v for k, v in cleaned.items() if v})
                    cleaned = existing
            new_models[upstream_name] = cleaned if cleaned else {}
            routes[alias] = f"{provider_key}/{upstream_name}"

        pdef["models"] = new_models

    # Merge with any existing routes (shouldn't exist in old format, but be safe).
    existing_routes = provider_block.get("routes", {})
    if existing_routes:
        for k, v in routes.items():
            existing_routes.setdefault(k, v)
    else:
        provider_block["routes"] = routes

    return data


def main() -> None:
    parser = argparse.ArgumentParser(description="Migrate MoonBridge config to new routes format.")
    parser.add_argument("input", nargs="?", default="config.yml", help="Input config file (default: config.yml)")
    parser.add_argument("output", nargs="?", default=None, help="Output file (default: overwrite input)")
    parser.add_argument("--dry-run", action="store_true", help="Print result to stdout without writing")
    args = parser.parse_args()

    input_path = Path(args.input)
    output_path = Path(args.output) if args.output else input_path

    if not input_path.exists():
        print(f"Error: {input_path} not found.", file=sys.stderr)
        sys.exit(1)

    yaml = YAML()
    yaml.preserve_quotes = True
    yaml.width = 4096  # Avoid unwanted line wrapping.

    with open(input_path) as f:
        data = yaml.load(f)

    if data is None:
        print(f"Error: {input_path} is empty or invalid YAML.", file=sys.stderr)
        sys.exit(1)

    migrate(data)

    if args.dry_run:
        yaml.dump(data, sys.stdout)
    else:
        with open(output_path, "w") as f:
            yaml.dump(data, f)
        print(f"Migrated config written to {output_path}")


if __name__ == "__main__":
    main()
