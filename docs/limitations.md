# Limitations

- This is not a sandbox: absolute paths and symlinks can reach the host.
- The view is best effort, not a frozen snapshot: a source file the user
  rewrites while the agent is reading it can show either version. Cache and
  local paths come from a snapshot of them taken in the background after the
  checkout opens: an edit the folder makes in the moments before the snapshot
  starts, or before the folder watcher reports one made earlier, can show.
- Where the state cannot share the folder's blocks (another volume, a
  filesystem without cloning) cache directories start empty in checkouts, and
  local paths are copied whole into each snapshot (see
  [configuration](configuration.md#classes)).
- A tracked file the user changes after open is read from the repository's
  objects; pruning those objects can make it unreadable.
- ovnfs reads git repositories with its own implementation, not a git
  installation. It reads the system configuration and attributes from
  `GIT_CONFIG_SYSTEM` or `/etc/gitconfig` and `/etc/gitattributes` (none on
  Windows), not from the prefix a particular git was installed to (such as
  Homebrew's or Git for Windows'), and cannot expand `%(prefix)/` paths.
  Fetches (`apply --branch`, the remote's git mirror) run `git fetch` when git
  is on `PATH`. Without git, ovnfs fetches itself over the same transports,
  finding credential helpers named in configuration on `PATH` or in
  `GIT_EXEC_PATH`, and reading client certificates (`http.sslCert`) only as
  PEM or DER files.
- Remote checkouts have no git directory and cannot be attached onward. Bring
  their edits back with `pull` on the source machine, then `apply` the local
  checkout to its folder. `fstat` on a lazily fetched open file reports the
  cached blob's metadata; path-based stat is exact.
- A link is one ssh session per checkout; cold reads of many files are bound by
  its throughput.
- Sockets and FIFOs can be made in NFS and FUSE checkouts, not SMB or WinFsp
  ones, and stay in the checkout: like a folder's own, they never reach
  `diff`, `apply` or a snapshot's tree. Device nodes are refused.
- NFSv3 locks are client-local. Explicit NFSv4 clients use byte-range locks
  held in the daemon's memory, lost when it restarts. SMB has no hard links
  and no durable handles.
- NFSv3 cannot push cache invalidations. The daemon refreshes its own mount's
  client for the paths a `pull` changed (see
  [platforms](platforms.md#nfs)), so those show at once; a reattach, which
  replaces whole trees, can remain cached below their roots until the client's
  attribute timeout (up to 60 s), as can any change on a machine that mounted
  the export itself. NFSv4 read delegations reduce that window for opened
  files, but do not cover directories or files only inspected with `stat`.
- One SMB daemon per Windows logon session: the SMB client keeps one set of
  credentials per server name, whatever the port, so a second daemon, whose
  state directory holds another password, cannot map `\\127.0.0.1\ovnfs`
  (error 1219). Run further daemons with `--proto winfsp`.
- WinFsp mounts get no recalls or change notifications for changes made
  behind them (`pull`, reattach): they show once the one-second
  metadata cache expires, and directory watchers miss them.
