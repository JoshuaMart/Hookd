# frozen_string_literal: true

module Hookd
  # Summarises a long-lived hook that currently has pending interactions,
  # returned by Client#activity.
  class HookActivity
    # last_seq is the newest seq held: at or below a cursor, nothing is new.
    attr_reader :hook, :pending_count, :last_interaction_at, :last_seq

    def initialize(hook:, pending_count:, last_interaction_at:, last_seq: nil)
      @hook = hook
      @pending_count = pending_count
      @last_interaction_at = last_interaction_at
      @last_seq = last_seq
    end

    # Create a HookActivity from an API response hash
    def self.from_hash(hash)
      raise ArgumentError, "Invalid hash: expected Hash, got #{hash.class}" unless hash.is_a?(Hash)

      new(
        hook: Hook.from_hash(hash['hook']),
        pending_count: hash['pending_count'],
        last_interaction_at: hash['last_interaction_at'],
        last_seq: hash['last_seq']
      )
    end

    def to_s
      "#<Hookd::HookActivity hook=#{hook.id} pending=#{pending_count}>"
    end
  end
end
