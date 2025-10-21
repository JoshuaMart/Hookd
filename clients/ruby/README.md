# Hookd Ruby Client

Ruby client library for [Hookd](https://github.com/JoshuaMart/hookd/server), a DNS/HTTP interaction server for security testing and debugging.

## Installation

Add this line to your application's Gemfile:

```ruby
gem 'hookd-client'
```

Or install it yourself as:

```bash
gem install hookd-client
```

## Usage

### Basic Example

```ruby
require 'hookd'
require 'typhoeus'

# Initialize the client
client = Hookd::Client.new(
  server: "https://hookd.example.com",
  token: ENV['HOOKD_TOKEN']
)

# Register a new hook
hook = client.register
puts "DNS endpoint: #{hook.dns}"
puts "HTTP endpoint: #{hook.http}"
puts "HTTPS endpoint: #{hook.https}"

# Make a request to the HTTP endpoint to simulate an interaction
Typhoeus.get(hook.http)

# Poll for interactions (single hook)
interactions = client.poll(hook.id)
interactions.each do |interaction|
  if interaction.dns?
    puts "DNS query: #{interaction.data}"
  elsif interaction.http?
    puts "HTTP request: #{interaction.data}"
  end
end
```

### Batch Polling Example

When working with multiple hooks, use `poll_batch` for better performance:

```ruby
require 'hookd'
require 'typhoeus'

# Initialize the client
client = Hookd::Client.new(
  server: "https://hookd.example.com",
  token: ENV['HOOKD_TOKEN']
)

# Register multiple hooks
puts "Registering 5 hooks..."
hooks = client.register(count: 5)

hook_ids = hooks.map(&:id)
puts "Created hooks: #{hook_ids.join(', ')}"

# Simulate some interactions...
# (make DNS queries, HTTP requests, etc.)

# Simulate some interactions
puts "\nSimulating HTTP requests..."
hooks.each do |hook|
  Typhoeus.get(hook.http)
  puts "  ✓ GET #{hook.http}"
end

# Batch poll all hooks at once (1 HTTP request instead of 5)
puts "Batch polling #{hook_ids.size} hooks..."
results = client.poll_batch(hook_ids)

# Display results
results.each do |hook_id, result|
  if result[:error]
    puts "❌ Hook #{hook_id}: #{result[:error]}"
  else
    interactions = result[:interactions]
    puts "✅ Hook #{hook_id}: #{interactions.size} interaction(s)"

    interactions.each do |interaction|
      if interaction.dns?
        puts "   - DNS: #{interaction.data['qname']} (#{interaction.data['qtype']})"
      elsif interaction.http?
        puts "   - HTTP: #{interaction.data['method']} #{interaction.data['path']}"
      end
    end
  end
end
```

### Configuration

The client requires two configuration parameters:

- `server`: The Hookd server URL (e.g., `https://hookd.example.com`)
- `token`: Authentication token for API access

### API Reference

#### `Hookd::Client`

Main client class for interacting with the Hookd server.

##### `#register(count: nil)`

Register one or more hooks and get DNS/HTTP endpoints.

**Single hook (default):**
```ruby
hook = client.register
# => #<Hookd::Hook id="abc123" dns="abc123.hookd.example.com" ...>
```

**Multiple hooks:**
```ruby
hooks = client.register(count: 5)
# => [#<Hookd::Hook id="abc123" ...>, #<Hookd::Hook id="def456" ...>, ...]
```

Parameters:
- `count` (Integer, optional) - Number of hooks to create (default: 1)

Returns:
- `Hookd::Hook` object when `count` is 1 or not specified
- Array of `Hookd::Hook` objects when `count` > 1

Raises:
- `ArgumentError` - Invalid count parameter
- `Hookd::AuthenticationError` - Authentication failed
- `Hookd::ServerError` - Server error (5xx)
- `Hookd::ConnectionError` - Connection failed

##### `#poll(hook_id)`

Poll for interactions captured by a single hook.

```ruby
interactions = client.poll("abc123")
# => [#<Hookd::Interaction type="dns" ...>, ...]
```

Parameters:
- `hook_id` (String) - The hook ID to poll

Returns: Array of `Hookd::Interaction` objects

Raises:
- `Hookd::AuthenticationError` - Authentication failed
- `Hookd::NotFoundError` - Hook not found
- `Hookd::ServerError` - Server error (5xx)
- `Hookd::ConnectionError` - Connection failed

##### `#poll_batch(hook_ids)`

**Batch poll** - Poll for interactions from multiple hooks in a single request.

```ruby
# Register multiple hooks
hooks = client.register(count: 3)
hook_ids = hooks.map(&:id)

# Batch poll all hooks at once (1 HTTP request instead of 3)
results = client.poll_batch(hook_ids)
# => {
#   "abc123" => { interactions: [...], error: nil },
#   "def456" => { interactions: [...], error: nil },
#   "ghi789" => { interactions: [], error: nil }
# }

# Process results
results.each do |hook_id, result|
  if result[:error]
    puts "Error for #{hook_id}: #{result[:error]}"
  else
    puts "Hook #{hook_id}: #{result[:interactions].size} interactions"
    result[:interactions].each do |interaction|
      puts "  - #{interaction.type}: #{interaction.data}"
    end
  end
end
```

Parameters:
- `hook_ids` (Array<String>) - Array of hook IDs to poll

Returns: Hash mapping hook IDs to results
- Each result contains:
  - `interactions` (Array<Hookd::Interaction>) - Array of interactions
  - `error` (String, nil) - Error message if hook not found

Raises:
- `ArgumentError` - Invalid hook_ids (not an array or empty)
- `Hookd::AuthenticationError` - Authentication failed
- `Hookd::ServerError` - Server error (5xx)
- `Hookd::ConnectionError` - Connection failed

**Benefits:**
- **Performance**: Reduced latency with single HTTP request
- **Efficiency**: Automatic connection reuse with HTTPX
- **Atomic**: Consistent snapshot of all hooks

##### `#metrics`

Get server metrics (requires authentication).

```ruby
metrics = client.metrics
# => {"total_hooks" => 42, "total_interactions" => 1337, ...}
```

Returns: Hash with metrics data

#### `Hookd::Hook`

Represents a registered hook with endpoints.

Attributes:
- `id` (String) - Unique hook identifier
- `dns` (String) - DNS endpoint
- `http` (String) - HTTP endpoint
- `https` (String) - HTTPS endpoint
- `created_at` (String) - Creation timestamp

#### `Hookd::Interaction`

Represents a captured DNS or HTTP interaction.

Attributes:
- `type` (String) - Interaction type ("dns" or "http")
- `timestamp` (String) - When the interaction was captured
- `data` (Hash) - Interaction details

Methods:
- `#dns?` - Returns true if this is a DNS interaction
- `#http?` - Returns true if this is an HTTP interaction

### Error Handling

The client raises specific exceptions for different error conditions:

```ruby
begin
  hook = client.register
rescue Hookd::AuthenticationError
  puts "Invalid token"
rescue Hookd::ConnectionError => e
  puts "Connection failed: #{e.message}"
rescue Hookd::ServerError => e
  puts "Server error: #{e.message}"
end
```

Exception hierarchy:
- `Hookd::Error` (base class)
  - `Hookd::AuthenticationError` - 401 Unauthorized
  - `Hookd::NotFoundError` - 404 Not Found
  - `Hookd::ServerError` - 5xx Server Error
  - `Hookd::ConnectionError` - Network/connection errors
