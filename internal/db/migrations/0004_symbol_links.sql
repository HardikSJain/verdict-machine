-- +goose Up
-- symbol_links: the canonical-entity overlay. NSE reissues an ISIN on a face-value
-- split, so one company owns several symbol_ids and its history snaps in half at the
-- split; bars is insert-only and every row carries the symbol_id assigned at insert
-- time, so the repair cannot be a rewrite. This table is the repair: a ~450-row map
-- from physical symbol_id to canonical entity_id, versioned on the same ingested_at
-- axis bars and symbols already use, joined by both readers. A wrong merge -- the one
-- irreversible outcome available here -- is corrected by inserting one retraction row,
-- and every answer given while the wrong link stood stays permanently replayable at its
-- own timestamp, which is what the project's reproducibility promise requires.
--
-- entity_id is drawn from the symbol_id space and is the chain root's id at mint time.
-- Resolution is ONE hop: a chain A->B->C is two rows both carrying entity_id = A.
-- predecessor and boundary are provenance for humans and for EntityBoundaries; the read
-- path never follows them.
CREATE TABLE symbol_links (
    symbol_id   bigint      NOT NULL,   -- the physical identity carried by bars rows
    entity_id   bigint      NOT NULL,   -- the canonical company
    reason      text        NOT NULL CHECK (reason IN ('succession', 'manual', 'retraction')),
    boundary    date,                   -- successor's first nse-bhavcopy session
    predecessor bigint,                 -- provenance only; NOT the resolution mechanism
    evidence    jsonb       NOT NULL,   -- every gate result and note the seeder saw
    roster_sha  bytea,                  -- sha256 of the reviewed roster that authorised this row
    seeded_by   text        NOT NULL,   -- 'entities apply v1' or a human's handle
    note        text,
    ingested_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (symbol_id, ingested_at),
    -- The shape constraint is on the ROW, not on the reason string. 'manual' was
    -- previously in the allow-list with nothing constraining it, so the riskiest rows in
    -- the table -- the hand-written INF fund-unit lines that §10 calls "exactly where a
    -- false positive would originate" -- were the only ones the schema declined to hold
    -- to the roster. Two silent consequences, both fatal to a load-bearing claim:
    -- roster_sha NULL breaks §5.1's provenance promise for precisely the rows that need
    -- it most, and boundary NULL makes the row invisible to entity_break (which filters
    -- `WHERE k.boundary IS NOT NULL`), so LastBreak comes back nil for an entity that
    -- genuinely has a boundary and §9's hard M1 prerequisite is silently satisfied for a
    -- NIFTYBEES-style AMC transfer. boundary is also how §4.3 chooses the in-force member,
    -- so a NULL boundary would put a manual member permanently out of the running for its
    -- own entity's label. reason now records provenance; it never relaxes a constraint.
    CHECK (reason = 'retraction' OR
           (predecessor IS NOT NULL AND boundary IS NOT NULL AND entity_id <> symbol_id
            AND roster_sha IS NOT NULL)),
    CHECK (reason <> 'retraction' OR
           (entity_id = symbol_id AND predecessor IS NULL AND boundary IS NULL)),
    CHECK (predecessor IS DISTINCT FROM symbol_id)
);
CREATE INDEX symbol_links_lookup_idx ON symbol_links (symbol_id, ingested_at DESC);
CREATE INDEX symbol_links_entity_idx ON symbol_links (entity_id, ingested_at DESC);

CREATE TRIGGER symbol_links_insert_only BEFORE UPDATE OR DELETE ON symbol_links
    FOR EACH ROW EXECUTE FUNCTION reject_mutation();

-- Flatness is enforced at write time, not merely checked afterwards. The resolver does
-- not follow hops, so a two-hop map would silently SPLIT one entity in two -- a wrong
-- answer with no error.
--
-- What this guarantees, stated exactly, because the guarantee is narrower than it looks:
-- the map AS RESOLVED AT now() -- the latest row per symbol -- stays flat. Both SELECTs
-- below read symbol_links with no bound relative to NEW.ingested_at, so what they inspect
-- is the map today's readers see, not the map a reader pinned at NEW.ingested_at sees.
-- Revision 1's comment here claimed the stronger "flat at every timestamp". That follows
-- only while every row's ingested_at is non-decreasing: then the map at any past pin is a
-- map this trigger already validated at the moment it was the current one, and induction
-- carries it. ingested_at is a settable column with a default, and one BACKDATED row
-- breaks the induction. Reproduced against this schema on 2026-09-08: insert B -> A at
-- 10:00, retract B at 10:01, then insert C -> B with ingested_at 10:00:30. The trigger
-- passes -- at now() B resolves to itself, so C -> B is one hop -- and entity_map_now is
-- flat, while entity_map_at('10:00:45') returns C -> B and B -> A, two hops, for a read
-- pinned inside that interval.
--
-- Bounding the trigger to NEW.ingested_at is NOT the fix: it would let a backdated row
-- pass a check the present map would fail, trading a past hole for a present one. Nothing
-- in Stage 1 writes a backdated row -- only the test helper can, and Stage 2's `apply`
-- takes the default -- so the map at a past pin is checked after the fact instead, by I1:
-- `verdict entities check --as-of-ingest <that timestamp>`. docs/DESIGN.md records the gap.
-- +goose StatementBegin
-- BOTH checks are guarded on the self-link case, and this is not cosmetic: revision 1's
-- unguarded version REJECTED THE DESIGN'S OWN UNDO. A retraction row is constrained to
-- entity_id = symbol_id, so for `retract --symbol 3` check 1 looked up symbol_id = 3,
-- found the very link it was undoing (3 -> 326), saw 326 <> 3 and raised
-- 'entity_id 3 itself resolves to 326'. Every linked symbol was un-retractable, which is
-- to say §2's "a wrong merge is undone by writing one more row" -- the single argument
-- that beat the ratified rebuild -- did not run. Check 2 then made the undo one-way: a
-- retraction row (3, 3) makes symbol 3 "the entity of" itself, so re-linking 3 -> 326
-- afterwards (the correction to a retraction that was itself the mistake, or a re-apply
-- after the roster is fixed) raised 'flat check2: 3 is the entity of other symbols',
-- permanently. Both were reproduced by evaluating this function's two SELECTs against a
-- simulated table; both are fixed below, and tests 8.4 and 8.6 now execute the inserts
-- rather than asserting on a fixture.
--
-- The premises are what the guards restore. Check 1 asks "is the thing you are pointing
-- at itself pointed elsewhere" -- meaningless for a symbol pointing at itself, which is a
-- root by definition and whose previous row is exactly what the retraction undoes. Check
-- 2 stops a PARTIAL re-point of OTHER members; a symbol is not another member of itself.
CREATE FUNCTION symbol_links_enforce_flat() RETURNS trigger AS $$
DECLARE
    resolved bigint;
BEGIN
    IF NEW.entity_id <> NEW.symbol_id THEN
        SELECT entity_id INTO resolved FROM symbol_links
         WHERE symbol_id = NEW.entity_id ORDER BY ingested_at DESC LIMIT 1;
        IF resolved IS NOT NULL AND resolved <> NEW.entity_id THEN
            RAISE EXCEPTION 'symbol_links: entity_id % itself resolves to %; the map is one hop, never transitive',
                NEW.entity_id, resolved;
        END IF;
        IF EXISTS (
            SELECT 1 FROM (
                SELECT DISTINCT ON (symbol_id) symbol_id, entity_id
                  FROM symbol_links ORDER BY symbol_id, ingested_at DESC
            ) m WHERE m.entity_id = NEW.symbol_id AND m.symbol_id <> NEW.symbol_id
        ) THEN
            RAISE EXCEPTION 'symbol_links: % is the entity of other symbols; re-point every member or none',
                NEW.symbol_id;
        END IF;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd
CREATE TRIGGER symbol_links_flat BEFORE INSERT ON symbol_links
    FOR EACH ROW EXECUTE FUNCTION symbol_links_enforce_flat();

-- entity_map_at(ts) is the PINNED resolution rule -- the same ingested_at <= ts predicate
-- the Go CTEs thread -- published for a Python notebook so it gets entities without
-- reimplementing the CTE. It is a function and not a view because revision 1 published
-- only an unpinned view, `entity_map_latest`, and §4.4 then told consumers to join
-- through it. That is the replay leak: a caller pins UniverseAsOf at asOfIngest = T, gets
-- an EntityID resolved at T, and joins it back to bars through a map resolved at now().
-- After any later split extends an entity, or any retraction dissolves one, the two
-- disagree silently and §7's "literally invisible to it" stops holding -- and the
-- snapshot_id amendment cannot catch it, because the row set is unchanged and only the
-- view's resolution moved.
-- +goose StatementBegin
CREATE FUNCTION entity_map_at(ts timestamptz)
RETURNS TABLE (symbol_id bigint, entity_id bigint) AS $$
    SELECT s.symbol_id, COALESCE(l.entity_id, s.symbol_id)
    FROM (SELECT DISTINCT symbols.symbol_id FROM symbols WHERE symbols.ingested_at <= ts) s
    LEFT JOIN LATERAL (
        SELECT symbol_links.entity_id FROM symbol_links
        WHERE symbol_links.symbol_id = s.symbol_id AND symbol_links.ingested_at <= ts
        ORDER BY symbol_links.ingested_at DESC LIMIT 1
    ) l ON true;
$$ LANGUAGE sql STABLE;
-- +goose StatementEnd

-- entity_map_now is the interactive convenience only. It is NEVER valid for replay: a
-- query pinned at an old asOfIngest must call entity_map_at(that timestamp). The name says
-- "now" so that a notebook author reading a join has to notice.
CREATE VIEW entity_map_now AS SELECT * FROM entity_map_at(now());

-- +goose Down
-- Down drops the READ SURFACE only. It deliberately does NOT drop symbol_links or its
-- rows, and this is a correction to revision 1, which dropped the table: §7 folds
-- symbol_links into snapshot_id, so once any run has recorded a snapshot_id a DROP TABLE
-- destroys that run's reproducibility permanently -- re-running `apply` mints fresh
-- ingested_at values that are not the ones the snapshot hashed. The advertised rollback
-- would have been the one operation the project's thesis cannot survive. Dropping the
-- view, the function and the flatness trigger is enough to revert every read to
-- per-symbol identity, because entity_map's COALESCE degrades to symbol_id when the Go
-- side stops asking. Removing the table itself is a separate, deliberate act and is
-- unavailable once a snapshot_id exists.
DROP VIEW entity_map_now;
DROP FUNCTION entity_map_at(timestamptz);
DROP TRIGGER symbol_links_flat ON symbol_links;
DROP FUNCTION symbol_links_enforce_flat();
-- DROP TABLE symbol_links;  -- see above: not part of Down.
