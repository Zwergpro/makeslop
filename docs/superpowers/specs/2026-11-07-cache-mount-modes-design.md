# Cache mount modes

## Intent
Allow users to choose read-only cache mounts with optional `:ro` / `:rw` suffixes. The default remains read-write. The user approved this short design in conversation.

## Behavior and constraints
- Shared/local syntax: `name[:ro|:rw]`; static syntax: `host:container[:ro|:rw]`.
- Strip the mode before existing path validation, normalization, duplicate detection, and destination conflict checks. Unknown modes and extra colon-separated fields fail.
- Preserve existing filesystem safeguards, ordering, and zero-value behavior. Missing managed sources are still prepared on real launches for read-only mounts; dry runs create nothing. Static sources must already exist.
- Propagate read-only through resolved mounts to SDK and dry-run output. No new Docker calls or dependencies.
- Non-goals: alternate YAML object syntax, changing source permissions, recursive read-only guarantees beyond existing Docker mount semantics, or allowing colons within paths.

## Approaches
The selected approach extends the existing string syntax and carries a parsed boolean through configuration and resolution. A separate YAML mode field would add unnecessary schema complexity; interpreting suffixes only in CLI resolution would leave configuration validation inconsistent.

## Builder decisions
Use typed managed declarations containing a normalized name and a read-only flag, and add a read-only flag to static declarations. Update existing internal fixtures to the typed representation. Add decoder, resolver, CLI integration, and Docker projection coverage, and update the cache reference and generated configuration comments where applicable.
