# Jobs used by the integration test.
require "sidekiq"

class SleepJob
  include Sidekiq::Job
  def perform(seconds) = sleep(seconds)
end

# Sent straight to the dead set.
class DeadJob
  include Sidekiq::Job
  sidekiq_options retry: 0
  def perform = raise("boom")
end

# Kept in the retry set.
class RetryJob
  include Sidekiq::Job
  sidekiq_options retry: 5
  def perform = raise("boom")
end
