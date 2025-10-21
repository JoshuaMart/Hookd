# frozen_string_literal: true

require 'httpx'
require 'json'

module Hookd
  # HTTP client for interacting with Hookd server
  class Client
    attr_reader :server, :token

    def initialize(server:, token:)
      @server = server
      @token = token
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
    # @return [Hookd::Hook, Array<Hookd::Hook>] single hook or array of hooks
    # @raise [Hookd::AuthenticationError] if authentication fails
    # @raise [Hookd::ServerError] if server returns 5xx
    # @raise [Hookd::ConnectionError] if connection fails
    # @raise [ArgumentError] if count is invalid
    def register(count: nil)
      body = count.nil? ? nil : { count: count }

      raise ArgumentError, 'count must be a positive integer' if count && (!count.is_a?(Integer) || count < 1)

      response = post('/register', body)

      # Single hook response (backward compatible)
      return Hook.from_hash(response) if response.key?('id')

      # Multiple hooks response
      return [] if response['hooks'].nil? || response['hooks'].empty?

      response['hooks'].map { |h| Hook.from_hash(h) }
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

    private

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

      body = response.body.to_s

      case response.status
      when 200, 201
        raise Error, 'Empty response body from server' if body.nil? || body.empty?

        JSON.parse(body)
      when 401
        raise AuthenticationError, "Authentication failed: #{body}"
      when 404
        raise NotFoundError, "Resource not found: #{body}"
      when 500..599
        raise ServerError, "Server error (#{response.status}): #{body}"
      else
        raise Error, "Unexpected response (#{response.status}): #{body}"
      end
    rescue JSON::ParserError => e
      raise Error, "Invalid JSON response: #{e.message}"
    end
  end
end
