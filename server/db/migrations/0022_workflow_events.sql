-- +goose Up

ALTER TABLE notifications DROP CONSTRAINT notifications_type_check;
ALTER TABLE notifications ADD CONSTRAINT notifications_type_check
  CHECK (type IN ('shoot_reminder','hold_expiry','auth_incomplete','gallery_deadline',
    'gallery_submitted','proof_ready','proof_changes_requested','proof_resubmitted',
    'proof_approved','proof_deadline','retouch_delivered','delivery_ready','archive_ready',
    'archive_retention_warning','archive_deleted','workflow_event'));

-- +goose Down

DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM notifications WHERE type = 'workflow_event') THEN
    RAISE EXCEPTION 'workflow event notifications exist; archive them before rolling back 0022';
  END IF;
END $$;

ALTER TABLE notifications DROP CONSTRAINT notifications_type_check;
ALTER TABLE notifications ADD CONSTRAINT notifications_type_check
  CHECK (type IN ('shoot_reminder','hold_expiry','auth_incomplete','gallery_deadline',
    'gallery_submitted','proof_ready','proof_changes_requested','proof_resubmitted',
    'proof_approved','proof_deadline','retouch_delivered','delivery_ready','archive_ready',
    'archive_retention_warning','archive_deleted'));
