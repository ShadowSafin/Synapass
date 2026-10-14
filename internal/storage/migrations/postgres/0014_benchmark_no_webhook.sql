-- Remove the generic webhook notification channel from the Benchmark Engine.
--
-- Rules pointing at the removed channel would be undeliverable, so they are
-- deleted first: deleting them keeps the replacement CHECK valid on every
-- environment, including ones that stored webhook rules while 0013 allowed
-- them. Email, Discord and Slack rules are untouched.

DELETE FROM benchmark_notifications WHERE channel = 'webhook';

ALTER TABLE benchmark_notifications DROP CONSTRAINT IF EXISTS benchmark_notifications_channel_check;
ALTER TABLE benchmark_notifications ADD CONSTRAINT benchmark_notifications_channel_check
    CHECK (channel IN ('email','discord','slack'));
