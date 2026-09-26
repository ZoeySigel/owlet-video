-- Read-only invariants for the isolated fixture after every queue has drained.
SELECT UTC_TIMESTAMP() AS checked_at;
SELECT status, COUNT(*) AS commands FROM interaction_commands GROUP BY status;
SELECT COUNT(*) AS pending_commands FROM interaction_commands WHERE completed_at IS NULL;
SELECT COUNT(*) AS pending_outbox FROM outboxes WHERE published_at IS NULL;
SELECT COUNT(*) AS comments, COUNT(DISTINCT body) AS unique_request_bodies
FROM comments WHERE body LIKE 'capacity 20% request %';
SELECT COUNT(*) AS expected_comment_notifications FROM comments c JOIN videos v ON v.id=c.video_id
WHERE c.body LIKE 'capacity 20% request %' AND c.user_id<>v.user_id;
SELECT kind, COUNT(*) AS notifications FROM notifications GROUP BY kind;
SHOW GLOBAL VARIABLES WHERE Variable_name IN ('sync_binlog','innodb_flush_log_at_trx_commit','binlog_group_commit_sync_delay');
