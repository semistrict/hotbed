#!/usr/bin/env bash
# Does a forked, already-built Rust workspace rebuild when the fork lives at a
# different path from the base? hotbed's design depends on the answer: if it
# rebuilds, every fork must be shown to its commands at the base's path.
#
# Usage: cargo-relocation-check.sh DIR    (DIR on btrfs; it is created and removed)
# Prints, for each case, how many units cargo compiled.
set -euo pipefail

dir=${1:?usage: $0 DIR}
mkdir -p "$dir"
trap 'cd /; for s in "$dir"/*; do btrfs subvolume delete "$s" >/dev/null 2>&1 || rm -rf "$s"; done; rmdir "$dir"' EXIT

btrfs subvolume create "$dir/base" >/dev/null
cd "$dir/base"
cat > Cargo.toml <<'EOF'
[workspace]
members = ["app", "core"]
resolver = "3"
EOF
cargo new -q --vcs none --lib core
cargo new -q --vcs none app
cat >> core/Cargo.toml <<'EOF'
serde = { version = "1", features = ["derive"] }
anyhow = "1"
EOF
cat >> app/Cargo.toml <<'EOF'
core = { path = "../core" }
EOF
cat > core/src/lib.rs <<'EOF'
#[derive(serde::Serialize, Debug)]
pub struct Lease { pub epoch: u64 }
pub fn lease() -> anyhow::Result<Lease> { Ok(Lease { epoch: 1 }) }
EOF
echo 'fn main() { println!("{:?}", core::lease().unwrap()); }' > app/src/main.rs
git init -q && git add -A && git -c user.email=x@x -c user.name=x commit -qm base

units() { cargo build --all-targets 2>&1 | grep -c '^ *Compiling' || true; }

echo "cold build at base path:        $(units) compiled"
echo "rebuild at base path:           $(units) compiled"

btrfs subvolume snapshot "$dir/base" "$dir/fork" >/dev/null
cd "$dir/fork"
echo "fork at another path, no edit:  $(units) compiled"
echo '// touched' >> app/src/main.rs
echo "fork, app edited:               $(units) compiled"
