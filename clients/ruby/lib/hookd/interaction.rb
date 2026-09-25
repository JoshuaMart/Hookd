# frozen_string_literal: true

module Hookd
  # Represents a captured DNS, HTTP or SMTP interaction
  class Interaction
    # seq increases per hook and is the cursor for Client#read and #ack.
    attr_reader :id, :seq, :type, :timestamp, :source_ip, :data

    def initialize(type:, timestamp:, source_ip:, data:, id: nil, seq: nil)
      @id = id
      @seq = seq
      @type = type
      @timestamp = timestamp
      @source_ip = source_ip
      @data = data
    end

    # Create an Interaction from API response hash
    def self.from_hash(hash)
      new(
        id: hash['id'],
        seq: hash['seq'],
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

    # Check if this is an SMTP interaction
    def smtp?
      type == 'smtp'
    end

    def to_s
      "#<Hookd::Interaction seq=#{seq} type=#{type} timestamp=#{timestamp} source_ip=#{source_ip}>"
    end

    def inspect
      "#<Hookd::Interaction:#{object_id.to_s(16)} @id=#{id.inspect}, @seq=#{seq.inspect}, " \
        "@type=#{type.inspect}, @timestamp=#{timestamp.inspect}, " \
        "@source_ip=#{source_ip.inspect}, @data=#{data.inspect}>"
    end
  end
end
