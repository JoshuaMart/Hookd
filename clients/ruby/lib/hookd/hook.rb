# frozen_string_literal: true

module Hookd
  # Represents a registered hook with DNS and HTTP endpoints. expires_at and
  # metadata are populated for long-lived hooks (registered with a ttl) and nil
  # otherwise.
  class Hook
    attr_reader :id, :dns, :http, :https, :created_at, :expires_at, :metadata

    def initialize(id:, dns:, http:, https:, created_at:, expires_at: nil, metadata: nil)
      @id = id
      @dns = dns
      @http = http
      @https = https
      @created_at = created_at
      @expires_at = expires_at
      @metadata = metadata
    end

    # Create a Hook from API response hash
    def self.from_hash(hash)
      raise ArgumentError, "Invalid hash: expected Hash, got #{hash.class}" unless hash.is_a?(Hash)

      new(
        id: hash['id'],
        dns: hash['dns'],
        http: hash['http'],
        https: hash['https'],
        created_at: hash['created_at'],
        expires_at: hash['expires_at'],
        metadata: hash['metadata']
      )
    end

    def to_s
      "#<Hookd::Hook id=#{id} dns=#{dns}>"
    end
  end
end
