# frozen_string_literal: true

module Hookd
  # Summarises a long-lived hook that currently has pending interactions,
  # returned by Client#activity.
  class HookActivity
    attr_reader :hook, :pending_count, :last_interaction_at

    def initialize(hook:, pending_count:, last_interaction_at:)
      @hook = hook
      @pending_count = pending_count
      @last_interaction_at = last_interaction_at
    end

    # Create a HookActivity from an API response hash
    def self.from_hash(hash)
      raise ArgumentError, "Invalid hash: expected Hash, got #{hash.class}" unless hash.is_a?(Hash)

      new(
        hook: Hook.from_hash(hash['hook']),
        pending_count: hash['pending_count'],
        last_interaction_at: hash['last_interaction_at']
      )
    end

    def to_s
      "#<Hookd::HookActivity hook=#{hook.id} pending=#{pending_count}>"
    end
  end
end
