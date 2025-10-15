# frozen_string_literal: true

module Hookd
  # Represents a captured DNS or HTTP interaction
  class Interaction
    attr_reader :type, :timestamp, :source_ip, :data

    def initialize(type:, timestamp:, source_ip:, data:)
      @type = type
      @timestamp = timestamp
      @source_ip = source_ip
      @data = data
    end

    # Create an Interaction from API response hash
    def self.from_hash(hash)
      new(
        type: hash['type'],
        timestamp: hash['timestamp'],
        source_ip: hash['source_ip'],
        data: hash['data']
      )
    end

    # Check if this is a DNS interaction
    def dns?
      type == 'dns'
    end

    # Check if this is an HTTP interaction
    def http?
      type == 'http'
    end

    def to_s
      "#<Hookd::Interaction type=#{type} timestamp=#{timestamp} source_ip=#{source_ip}>"
    end

    def inspect
      "#<Hookd::Interaction:#{object_id.to_s(16)} @type=#{type.inspect}, @timestamp=#{timestamp.inspect}, " \
        "@source_ip=#{source_ip.inspect}, @data=#{data.inspect}>"
    end
  end
end
