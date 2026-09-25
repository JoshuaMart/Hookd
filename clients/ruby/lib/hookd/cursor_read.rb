# frozen_string_literal: true

module Hookd
  # Result of a non-destructive Client#read
  class CursorRead
    attr_reader :interactions, :dropped_through, :metadata

    def initialize(interactions:, dropped_through:, metadata: nil)
      @interactions = interactions
      @dropped_through = dropped_through
      @metadata = metadata
    end

    # True when the server evicted interactions past the cursor before they
    # were acknowledged.
    def lost?(after)
      dropped_through > after
    end

    def to_s
      "#<Hookd::CursorRead interactions=#{interactions.size} dropped_through=#{dropped_through}>"
    end
  end
end
