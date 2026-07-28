-- +goose Up
-- Repair: 57 rows that each carry their own eeid were merged into ANOTHER row that also
-- carries an eeid. An eeid identifies exactly one employee, so every one of those merges
-- collapsed two different people — in chains (Pal -> Parhi -> Patel -> Pathrave), because a
-- name-match merge accepted the placeholder display_name "A" as a unique full name.
--
-- Safe to undo: all 57 losers have email IS NULL (nothing was transferred off them), and
-- only 3 graph.nodes reference any row involved, so no attribution is rebuilt here.
-- mergePersonInto now refuses two eeid-bearing rows, and name merges require a
-- first+last name, so this cannot recur.
UPDATE graph.people l
SET merged_into = NULL
FROM graph.people s
WHERE s.id = l.merged_into
  AND l.eeid IS NOT NULL
  AND s.eeid IS NOT NULL;

-- The placeholder that caused it: blank it so the next BambooHR import fills the real
-- name, and so no future name match can ever key on "A" again.
UPDATE graph.people SET display_name = '' WHERE display_name = 'A';

-- +goose Down
-- Deliberately empty: re-collapsing distinct employees is data loss, not a rollback.
SELECT 1;
