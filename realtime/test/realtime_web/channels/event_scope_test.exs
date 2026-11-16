defmodule RealtimeWeb.EventScopeTest do
  use RealtimeWeb.ChannelCase, async: true

  alias Phoenix.Socket.Message
  alias Realtime.Fanout
  alias RealtimeWeb.Channels.EventScope
  alias RealtimeWeb.WorkspaceChannel

  defp envelope(wid, type) do
    %{
      "id" => Tokens.uuid(),
      "seq" => System.unique_integer([:positive]),
      "workspace_id" => wid,
      "type" => type,
      "visibility" => "agents",
      "topics" => ["workspace:#{wid}"],
      "data" => %{"secret" => "content"},
      "occurred_at" => "2026-10-11T09:00:00Z"
    }
  end

  defp join_key(events) do
    claims = Tokens.ws_claims(%{"kind" => "api_key", "events" => events}) |> Map.delete("sid")
    {:ok, socket} = connect_claims(claims)
    wid = claims["wid"]
    {:ok, _, _} = subscribe_and_join(socket, WorkspaceChannel, "workspace:#{wid}")
    wid
  end

  defp publish(env), do: Fanout.handle("od.events." <> env["workspace_id"], Jason.encode!(env))

  test "a key that reads tickets only gets other events as the stand-in" do
    wid = join_key(%{"only" => true, "resources" => ["message", "ticket"]})
    ws = "workspace:#{wid}"

    ticket = envelope(wid, "ticket.updated")
    assert {:ok, _} = publish(ticket)
    full = Map.delete(ticket, "topics")
    assert_receive %Message{topic: ^ws, event: "event", payload: ^full}

    contact = envelope(wid, "contact.updated")
    assert {:ok, _} = publish(contact)
    stand_in = Fanout.hidden(contact)
    assert_receive %Message{topic: ^ws, event: "event", payload: ^stand_in}
    refute_receive %Message{}, 50
  end

  test "a key that reads settings gets everything but the listed resources" do
    wid = join_key(%{"only" => false, "resources" => ["contact"]})
    ws = "workspace:#{wid}"

    workspace = envelope(wid, "workspace.updated")
    assert {:ok, _} = publish(workspace)
    full = Map.delete(workspace, "topics")
    assert_receive %Message{topic: ^ws, event: "event", payload: ^full}

    contact = envelope(wid, "contact.created")
    assert {:ok, _} = publish(contact)
    assert_receive %Message{topic: ^ws, event: "event", payload: %{"type" => "event.hidden"} = p}
    refute Map.has_key?(p["data"], "secret")
  end

  test "allowed?/2" do
    only = %{only: true, resources: MapSet.new(["ticket"])}
    assert EventScope.allowed?(only, "ticket.created")
    refute EventScope.allowed?(only, "ticket_link.created")
    refute EventScope.allowed?(only, "contact.created")
    assert EventScope.allowed?(only, "event.hidden")
    refute EventScope.allowed?(only, nil)

    except = %{only: false, resources: MapSet.new(["contact"])}
    assert EventScope.allowed?(except, "member.added")
    refute EventScope.allowed?(except, "contact.deleted")
  end
end
