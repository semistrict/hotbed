# Global sync(2) to make ZFS block counts current in tests

## Tried
Calling `syscall.Sync()` in tests before reading `st_blocks` (board 7674, a duplicate of board 7650), because ZFS counts a write's blocks (and a punched hole's) only when its transaction group commits, seconds later, and neither `fsync` nor `syncfs` waits for that.

## Idea
`sync(2)` forces every pool's txg to commit, so `st_blocks` is current right after.

## Result
On a busy host with other runs' VMs and builds, the test sat for 10+ minutes in uninterruptible sleep (`sync_inodes_sb`, `wb_wait_for_completion`) flushing everyone else's dirty data.

## Why it's dead
A test must not wait on the whole machine's writeback. Nor need it: what a log holds is read without `st_blocks` at all, by its data extents (`cmdlog.DataBytes`, `SEEK_DATA`/`SEEK_HOLE`, current at once on ZFS, btrfs, ext4 and APFS), and pruning budgets by kept bytes (`cmdlog.KeptBytes`; board 7650). Seeking to a hole on ZFS also waits for just that file's pool to commit (`zfs_dmu_offset_next_sync`), about a second, should `st_blocks` itself ever be needed.

## Reopen if
`zfs_dmu_offset_next_sync` is off on a node, or a filesystem lags `st_blocks` and ignores `SEEK_HOLE`.
