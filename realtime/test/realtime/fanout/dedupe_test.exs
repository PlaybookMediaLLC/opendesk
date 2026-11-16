defmodule Realtime.Fanout.DedupeTest do
  # Not async: the sweep forgets every remembered envelope, other tests' too.
  use ExUnit.Case, async: false

  alias Realtime.Fanout.Dedupe
  alias Realtime.Test.Tokens

  test "an envelope is dropped only when its id was seen before" do
    wid = Tokens.uuid()
    first = %{"id" => Tokens.uuid(), "seq" => 10}

    assert Dedupe.check(wid, first) == :ok
    assert Dedupe.check(wid, first) == :duplicate
    # Another event with a lower seq is not a repeat.
    assert Dedupe.check(wid, %{"id" => Tokens.uuid(), "seq" => 9}) == :ok
    assert Dedupe.check(wid, %{"id" => Tokens.uuid(), "seq" => 10}) == :ok
  end

  test "without an id, the workspace and seq identify the event" do
    wid = Tokens.uuid()
    assert Dedupe.check(wid, %{"seq" => 5}) == :ok
    assert Dedupe.check(wid, %{"seq" => 5}) == :duplicate
    assert Dedupe.check(wid, %{"seq" => 4}) == :ok
    assert Dedupe.check(Tokens.uuid(), %{"seq" => 5}) == :ok
  end

  test "an envelope without an id or seq is never dropped" do
    wid = Tokens.uuid()
    assert Dedupe.check(wid, %{}) == :ok
    assert Dedupe.check(wid, %{}) == :ok
  end

  test "a remembered id is forgotten after the window" do
    wid = Tokens.uuid()
    env = %{"id" => Tokens.uuid(), "seq" => 1}
    assert Dedupe.check(wid, env) == :ok
    Dedupe.sweep(System.monotonic_time(:millisecond) + 600_001)
    assert Dedupe.check(wid, env) == :ok
  end
end
