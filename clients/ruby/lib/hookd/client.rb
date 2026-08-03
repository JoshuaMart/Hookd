# frozen_string_literal: true

require 'httpx'
require 'json'

module Hookd
  # HTTP client for interacting with Hookd server
  class Client
    # Caps the payload materialised as a String. Generous, since a poll can
    # return many interactions with full bodies. Zero or less disables it.
    DEFAULT_MAX_RESPONSE_BYTES = 64 * 1024 * 1024

    # How much of an error response is quoted back in the raised message.
    ERROR_BODY_EXCERPT_BYTES = 1024

    attr_reader :server, :token, :max_response_bytes

    def initialize(server:, token:, max_response_bytes: DEFAULT_MAX_RESPONSE_BYTES)
      @server = server
      @token = token
      @max_response_bytes = max_response_bytes
      @http = HTTPX.with(
        headers: { 'X-API-Key' => token },
        timeout: {
          connect_timeout: 10,
          read_timeout: 30
        }
      )
    end

    # Register one or more hooks
    # @param count [Integer, nil] number of hooks to register (default: 1)
    # @param ttl [String, nil] lifetime as a Go duration ("168h") or day count
    #   ("7d"); a value above the server's ephemeral hook_ttl registers a durable
    #   long-lived hook. Omit for an ephemeral hook.
    # @param metadata [Hash, nil] arbitrary data stored with the hook and echoed
    #   back on poll
    # @return [Hookd::Hook, Array<Hookd::Hook>] single hook or array of hooks
    # @raise [Hookd::AuthenticationError] if authentication fails
    # @raise [Hookd::ServerError] if server returns 5xx
    # @raise [Hookd::ConnectionError] if connection fails
    # @raise [ArgumentError] if count is invalid
    def register(count: nil, ttl: nil, metadata: nil)
      raise ArgumentError, 'count must be a positive integer' if count && (!count.is_a?(Integer) || count < 1)

      parse_register_response(post('/register', register_body(count, ttl, metadata)))
    end

    # Poll for interactions on a hook
    # @param hook_id [String] the hook ID to poll
    # @return [Array<Hookd::Interaction>] array of interactions (may be empty)
    # @raise [Hookd::AuthenticationError] if authentication fails
    # @raise [Hookd::NotFoundError] if hook not found
    # @raise [Hookd::ServerError] if server returns 5xx
    # @raise [Hookd::ConnectionError] if connection fails
    def poll(hook_id)
      response = get("/poll/#{hook_id}")

      # Response is {"interactions": [...]}
      interactions = response['interactions']
      return [] if interactions.nil? || interactions.empty? || !interactions.is_a?(Array)

      interactions.map { |i| Interaction.from_hash(i) }
    rescue NoMethodError => e
      raise Error, "Invalid response format: #{e.message}"
    end

    # Poll for interactions on multiple hooks (batch)
    # @param hook_ids [Array<String>] the hook IDs to poll
    # @return [Hash<String, Hash>] hash mapping hook_id to results
    #   Results format: { "hook_id" => { interactions: [...], error: "..." } }
    # @raise [Hookd::AuthenticationError] if authentication fails
    # @raise [Hookd::ServerError] if server returns 5xx
    # @raise [Hookd::ConnectionError] if connection fails
    # @raise [ArgumentError] if hook_ids is invalid
    def poll_batch(hook_ids)
      validate_hook_ids(hook_ids)

      url = "#{@server}/poll"
      options = { headers: { 'Content-Type' => 'application/json' }, json: hook_ids }
      response = @http.post(url, **options)
      response_data = handle_response(response)

      transform_batch_results(response_data['results'])
    rescue NoMethodError => e
      raise Error, "Invalid response format: #{e.message}"
    end

    # Get server metrics (requires authentication)
    # @return [Hash] metrics data
    # @raise [Hookd::AuthenticationError] if authentication fails
    # @raise [Hookd::ServerError] if server returns 5xx
    # @raise [Hookd::ConnectionError] if connection fails
    def metrics
      get('/metrics')
    end

    # List long-lived hooks that currently have pending interactions, so you can
    # discover which of your long-lived hooks fired without polling each one.
    # Drain the details with #poll. Returns an empty array when none have fired
    # (or the server has long-lived hooks disabled).
    # @return [Array<Hookd::HookActivity>]
    # @raise [Hookd::AuthenticationError] if authentication fails
    # @raise [Hookd::ServerError] if server returns 5xx
    # @raise [Hookd::ConnectionError] if connection fails
    def activity
      response = get('/activity')

      hooks = response['hooks']
      return [] if hooks.nil? || hooks.empty? || !hooks.is_a?(Array)

      hooks.map { |h| HookActivity.from_hash(h) }
    rescue NoMethodError => e
      raise Error, "Invalid response format: #{e.message}"
    end

    private

    def register_body(count, ttl, metadata)
      body = {}
      body[:count] = count unless count.nil?
      body[:ttl] = ttl unless ttl.nil?
      body[:metadata] = metadata unless metadata.nil?
      body.empty? ? nil : body
    end

    def parse_register_response(response)
      # Single hook response (backward compatible)
      return Hook.from_hash(response) if response.key?('id')

      # Multiple hooks response
      return [] if response['hooks'].nil? || response['hooks'].empty?

      response['hooks'].map { |h| Hook.from_hash(h) }
    end

    def get(path)
      url = "#{@server}#{path}"
      response = @http.get(url)
      handle_response(response)
    end

    def post(path, body = nil)
      url = "#{@server}#{path}"
      options = { headers: { 'Content-Type' => 'application/json' } }
      options[:json] = body if body

      response = @http.post(url, **options)
      handle_response(response)
    end

    def validate_hook_ids(hook_ids)
      raise ArgumentError, 'hook_ids must be an array' unless hook_ids.is_a?(Array)
      raise ArgumentError, 'hook_ids cannot be empty' if hook_ids.empty?
    end

    def transform_batch_results(results)
      return {} if results.nil? || !results.is_a?(Hash)

      results.transform_values do |result|
        {
          interactions: result['error'] ? [] : map_interactions(result['interactions']),
          error: result['error']
        }
      end
    end

    def map_interactions(interactions)
      return [] if interactions.nil?

      interactions.map { |i| Interaction.from_hash(i) }
    end

    def handle_response(response)
      # HTTPX returns HTTPX::ErrorResponse for connection/timeout errors
      if response.is_a?(HTTPX::ErrorResponse)
        error = response.error
        raise ConnectionError, "Connection failed: #{error.message}"
      end

      case response.status
      when 200, 201
        parse_body(response)
      when 401
        raise AuthenticationError, "Authentication failed: #{error_excerpt(response)}"
      when 404
        raise NotFoundError, "Resource not found: #{error_excerpt(response)}"
      when 500..599
        raise ServerError, "Server error (#{response.status}): #{error_excerpt(response)}"
      else
        raise Error, "Unexpected response (#{response.status}): #{error_excerpt(response)}"
      end
    end

    def parse_body(response)
      body = read_body(response, @max_response_bytes)
      raise Error, 'Empty response body from server' if body.empty?

      JSON.parse(body)
    rescue JSON::ParserError => e
      raise Error, "Invalid JSON response: #{e.message}"
    end

    # Accumulates chunk by chunk and stops at the ceiling, raising unless
    # truncate is set, in which case it returns the bounded slice.
    def read_body(response, limit, truncate: false)
      return response.body.to_s if limit <= 0

      body = nil
      response.body.each do |chunk|
        # Seed from the first chunk to keep the payload's own encoding.
        body = body ? body << chunk : chunk.dup
        next if body.bytesize <= limit
        return body.byteslice(0, limit).scrub if truncate

        raise ResponseTooLargeError, "Response exceeds the #{limit} byte limit"
      end
      body || ''
    end

    # A large error page must not become a large exception message.
    def error_excerpt(response)
      read_body(response, ERROR_BODY_EXCERPT_BYTES, truncate: true)
    end
  end
end
