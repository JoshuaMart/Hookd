# frozen_string_literal: true

module Hookd
  # Represents a registered hook with DNS and HTTP endpoints
  class Hook
    attr_reader :id, :dns, :http, :https, :created_at

    def initialize(id:, dns:, http:, https:, created_at:)
      @id = id
      @dns = dns
      @http = http
      @https = https
      @created_at = created_at
    end

    # Create a Hook from API response hash
    def self.from_hash(hash)
      raise ArgumentError, "Invalid hash: expected Hash, got #{hash.class}" unless hash.is_a?(Hash)

      new(
        id: hash['id'],
        dns: hash['dns'],
        http: hash['http'],
        https: hash['https'],
        created_at: hash['created_at']
      )
    end

    def to_s
      "#<Hookd::Hook id=#{id} dns=#{dns}>"
    end
  end
end
