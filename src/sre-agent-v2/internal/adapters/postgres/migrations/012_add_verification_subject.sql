ALTER TABLE verifications
    ADD COLUMN subject_cluster TEXT NOT NULL,
    ADD COLUMN subject_namespace TEXT NOT NULL,
    ADD COLUMN subject_kind TEXT NOT NULL,
    ADD COLUMN subject_name TEXT NOT NULL,
    ADD COLUMN subject_uid TEXT NOT NULL,
    ADD CONSTRAINT verifications_subject_cluster_check
        CHECK (btrim(subject_cluster) <> ''),
    ADD CONSTRAINT verifications_subject_namespace_check
        CHECK (btrim(subject_namespace) <> ''),
    ADD CONSTRAINT verifications_subject_kind_check
        CHECK (btrim(subject_kind) <> ''),
    ADD CONSTRAINT verifications_subject_name_check
        CHECK (btrim(subject_name) <> ''),
    ADD CONSTRAINT verifications_subject_uid_check
        CHECK (btrim(subject_uid) <> '');

---- create above / drop below ----

ALTER TABLE verifications
    DROP CONSTRAINT verifications_subject_uid_check,
    DROP CONSTRAINT verifications_subject_name_check,
    DROP CONSTRAINT verifications_subject_kind_check,
    DROP CONSTRAINT verifications_subject_namespace_check,
    DROP CONSTRAINT verifications_subject_cluster_check,
    DROP COLUMN subject_uid,
    DROP COLUMN subject_name,
    DROP COLUMN subject_kind,
    DROP COLUMN subject_namespace,
    DROP COLUMN subject_cluster;
