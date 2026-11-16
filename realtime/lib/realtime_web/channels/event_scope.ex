defmodule RealtimeWeb.Channels.EventScope do
  @moduledoc """
  What a socket receives of each `"event"` envelope, by the token's `events`
  claim (`contracts/tokens.md`). A scoped credential (an API key, or a person
  acting through an OAuth grant) receives in full only the events its scopes
  read; every other event reaches it as the `event.hidden` stand-in, so `seq`
  stays contiguous. A token without the claim (a person signed in) receives
  every envelope as the channel got it.

  An event's resource is its type up to the first dot (`ticket.updated` →
  `ticket`). With `only`, the claim lists the resources received in full; without
  it, the resources that are not. The API decides the list from its one table of
  event scopes; the gateway only applies it.
  """

  alias Realtime.Fanout

  @doc "The envelope `socket` receives for `payload`."
  @spec visible(map(), Phoenix.Socket.t()) :: map()
  def visible(payload, %{assigns: assigns}) do
    case Map.get(assigns, :events) do
      nil -> payload
      filter -> if allowed?(filter, payload["type"]), do: payload, else: Fanout.hidden(payload)
    end
  end

  @doc "Whether an event of `type` passes `filter` in full."
  @spec allowed?(%{only: boolean(), resources: MapSet.t()}, term()) :: boolean()
  def allowed?(_filter, "event.hidden"), do: true

  def allowed?(%{only: only, resources: resources}, type) when is_binary(type) do
    [resource | _] = String.split(type, ".", parts: 2)
    MapSet.member?(resources, resource) == only
  end

  def allowed?(_filter, _type), do: false
end
