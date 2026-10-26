-- Agent configuration publication metadata: the catalog snapshot carries each listing's
-- declaration association and its validated static copy, so configuration reads never need the
-- marketplace checkout, the release package, or plugin code.
--
-- agent_configuration is set only when the listing declared an [agent_configuration] section and
-- the static copy validated against it (digest, compile, version). It stores the association
-- (schema version, declaration SHA-256, copy path) plus the exact copy bytes, so the persisted
-- snapshot is self-contained and byte-faithful.
--
-- agent_configuration_issue records the bounded reason a declared association could not be
-- consumed. It stays NULL for listings that declare nothing: "not declared" and "declared but
-- invalid" are different facts, and only the latter blocks personal configuration while the
-- package itself remains installable.
ALTER TABLE plugin_catalog_entries ADD COLUMN agent_configuration jsonb;
ALTER TABLE plugin_catalog_entries ADD COLUMN agent_configuration_issue text;
