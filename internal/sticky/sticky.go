// Package sticky provides the overcomplicated Java sticky partitioning
// strategy for Kafka, with modifications made to be stickier and fairer.
//
// For some points on how Java's strategy is flawed, see
// https://github.com/Shopify/sarama/pull/1416/files/b29086bdaae0da7ce71eae3f854d50685fd6b631#r315005878
package sticky

// Give each member in same rung to steal one,

import (
	"fmt"
	"reflect"
	"sort"

	"github.com/google/btree"

	"github.com/twmb/kgo/kmsg"
)

// Sticky partitioning has two versions, the latter from KIP-341 preventing a
// bug. The second version introduced generations with the default generation
// from the first generation's consumers defaulting to -1.

const defaultGeneration = -1

type GroupMember struct {
	ID string

	Version  int16
	Topics   []string
	UserData []byte
}

type Plan map[string]map[string][]int32

type balancer struct {
	// members are the members in play for this balance.
	//
	// This is built in newBalancer mapping member IDs to the GroupMember.
	members map[string]GroupMember

	// topics are the topic names and partitions that the client knows of
	// and passed to be used for balancing.
	//
	// This is repeatedly used for filtering topics that members indicate
	// they can consume but that our client does not know of.
	topics map[string][]int32

	// plan is the plan that we are building to balance partitions.
	//
	// This is initialized with data from the userdata each group member
	// is sending with the join. After, we use this to move partitions
	// around or assign new partitions.
	plan membersPartitions

	// planByNumPartitions orders plan member partitions by the number of
	// partitions each member is consuming.
	//
	// The nodes in the btree reference values in plan, meaning updates in
	// this field are visible in plan.
	planByNumPartitions *btree.BTree

	// isFreshAssignment tracks whether this is the first join for a group.
	// This is true if no member has userdata (plan is empty)
	isFreshAssignment bool
	// areSubscriptionsIdentical tracks if every member can consume the
	// same partitions. If true, this makes the isBalanced check much
	// simpler.
	areSubscriptionsIdentical bool

	// partitionConsumers maps all possible partitions to consume to the
	// members that are consuming them.
	//
	// We initialize this from our plan and modify it during reassignment.
	// We use this to know what member we are stealing partitions from.
	partitionConsumers map[topicPartition]string

	// consumers2AllPotentialPartitions maps each member to all of the
	// partitions it theoretically could consume. This is repeatedly used
	// during assignment to see if a partition we want to move can be moved
	// to a member.
	//
	// (maps each partition => each member that could consume it)
	//
	// This is built once and never modified thereafter.
	consumers2AllPotentialPartitions staticMembersPartitions

	// partitions2AllPotentialConsumers maps each partition to a member
	// that could theoretically consume it. This is repeatedly used during
	// assignment to see which members could consume a partition we want to
	// move.
	//
	// (maps each member => each partition it could consume)
	//
	// This is built once and never modified thereafter.
	partitions2AllPotentialConsumers staticPartitionMembers
}

type topicPartition struct {
	topic     string
	partition int32
}

func newBalancer(members []GroupMember, topics map[string][]int32) *balancer {
	b := &balancer{
		members: make(map[string]GroupMember, len(members)),
		topics:  topics,

		plan: make(membersPartitions),

		partitionConsumers: make(map[topicPartition]string),

		partitions2AllPotentialConsumers: make(staticPartitionMembers),
		consumers2AllPotentialPartitions: make(staticMembersPartitions),
	}
	for _, member := range members {
		b.members[member.ID] = member
	}
	return b
}

func (b *balancer) into() Plan {
	plan := make(Plan)
	for member, partitions := range b.plan {
		topics, exists := plan[member]
		if !exists {
			topics = make(map[string][]int32)
			plan[member] = topics
		}
		for _, partition := range *partitions {
			topics[partition.topic] = append(topics[partition.topic], partition.partition)
		}
	}
	return plan
}

// staticMembersPartitions is like membersPartitions below, but is used only
// for consumers2AllPotentialPartitions. The value is built once and never
// changed. Essentially, this is a clearer type.
type staticMembersPartitions map[string]map[topicPartition]struct{}

// membersPartitions maps members to a pointer of their partitions.  We use a
// pointer so that modifications through memberWithPartitions can be seen in
// any membersPartitions map.
type membersPartitions map[string]*[]topicPartition

// memberWithPartitions ties a member to a pointer to its partitions.
//
// This is generally used for sorting purposes.
type memberWithPartitions struct {
	member     string
	partitions *[]topicPartition
}

func (l memberWithPartitions) less(r memberWithPartitions) bool {
	return len(*l.partitions) < len(*r.partitions) ||
		len(*l.partitions) == len(*r.partitions) &&
			l.member < r.member
}

func (l memberWithPartitions) Less(r btree.Item) bool {
	return l.less(r.(memberWithPartitions))
}

func (m membersPartitions) intoConsumersPartitions() []memberWithPartitions {
	var consumersPartitions []memberWithPartitions
	for member, partitions := range m {
		consumersPartitions = append(consumersPartitions, memberWithPartitions{
			member,
			partitions,
		})
	}
	return consumersPartitions
}

func (m membersPartitions) btreeByConsumersPartitions() *btree.BTree {
	bt := btree.New(8)
	for _, memberWithPartitions := range m.intoConsumersPartitions() {
		bt.ReplaceOrInsert(memberWithPartitions)
	}
	return bt
}

func (mps membersPartitions) deepClone() membersPartitions {
	clone := make(membersPartitions, len(mps))
	for member, partitions := range mps {
		dup := append([]topicPartition(nil), *partitions...)
		clone[member] = &dup
	}
	return clone
}

// staticPartitionMember is the same as partitionMembers, but we type name it
// to imply immutability in reading. All mutable uses go through cloneKeys
// or shallowClone.
type staticPartitionMembers map[topicPartition]map[string]struct{}

func (orig staticPartitionMembers) cloneKeys() map[topicPartition]struct{} {
	dup := make(map[topicPartition]struct{}, len(orig))
	for partition := range orig {
		dup[partition] = struct{}{}
	}
	return dup
}

func Balance(members []GroupMember, topics map[string][]int32) Plan {
	// Code below relies on members to be sorted. It should be: that is the
	// contract of the Balance interface. But, just in case.
	sort.Slice(members, func(i, j int) bool { return members[i].ID < members[j].ID })

	b := newBalancer(members, topics)

	// Parse the member metadata for figure out what everybody was doing.
	b.parseMemberMetadata()
	b.initAllConsumersPartitions()
	// For planByNumPartitions, we use a btree heap since we will be
	// accessing both the min and max often as well as ranging from
	// smallest to largest.
	//
	// We init this after initAllConsumersPartitions, which can add new
	// members that were not in the prior plan.
	b.planByNumPartitions = b.plan.btreeByConsumersPartitions()
	b.assignUnassignedPartitions()

	b.balance()

	return b.into()
}

func strsHas(search []string, needle string) bool {
	for _, check := range search {
		if check == needle {
			return true
		}
	}
	return false
}

// parseMemberMetadata parses all member userdata to initialize the prior plan.
func (b *balancer) parseMemberMetadata() {
	type memberGeneration struct {
		member     string
		generation int32
	}

	// all partitions => members that are consuming those partitions
	// Each partition should only have one consumer, but a flaky member
	// could rejoin with an old generation (stale user data) and say it
	// is consuming something a different member is. See KIP-341.
	partitionConsumersByGeneration := make(map[topicPartition][]memberGeneration)

	for _, member := range b.members {
		memberPlan, generation := deserializeUserData(member.Version, member.UserData)
		memberGeneration := memberGeneration{
			member.ID,
			generation,
		}
		fmt.Println("deserialized", memberPlan, generation)
		for _, topicPartition := range memberPlan {
			partitionConsumers := partitionConsumersByGeneration[topicPartition]
			var doublyConsumed bool
			for _, otherConsumer := range partitionConsumers { // expected to be very few if any others
				if otherConsumer.generation == generation {
					doublyConsumed = true
					break
				}
			}
			// Two members should not be consuming the same topic and partition
			// within the same generation. If see this, we drop the second.
			if doublyConsumed {
				continue
			}
			partitionConsumers = append(partitionConsumers, memberGeneration)
			partitionConsumersByGeneration[topicPartition] = partitionConsumers
		}
	}

	for partition, partitionConsumers := range partitionConsumersByGeneration {
		sort.Slice(partitionConsumers, func(i, j int) bool {
			return partitionConsumers[i].generation > partitionConsumers[j].generation
		})

		member := partitionConsumers[0].member
		memberPartitions := b.plan[member]
		if memberPartitions == nil {
			memberPartitions = new([]topicPartition)
			b.plan[member] = memberPartitions
		}
		*memberPartitions = append(*memberPartitions, partition)
	}

	b.isFreshAssignment = len(b.plan) == 0
}

// deserializeUserData returns the topic partitions a member was consuming and
// the join generation it was consuming from.
//
// If anything fails or we do not understand the userdata parsing generation,
// we return empty defaults. The member will just be assumed to have no
// history.
func deserializeUserData(version int16, userdata []byte) (memberPlan []topicPartition, generation int32) {
	generation = defaultGeneration
	switch version {
	case 0:
		var v0 kmsg.StickyMemberMetadataV0
		if err := v0.ReadFrom(userdata); err != nil {
			return nil, 0
		}
		for _, topicAssignment := range v0.CurrentAssignment {
			for _, partition := range topicAssignment.Partitions {
				memberPlan = append(memberPlan, topicPartition{
					topicAssignment.Topic,
					partition,
				})
			}
		}
	case 1:
		var v1 kmsg.StickyMemberMetadataV1
		if err := v1.ReadFrom(userdata); err != nil {
			return nil, 0
		}
		generation = v1.Generation
		for _, topicAssignment := range v1.CurrentAssignment {
			for _, partition := range topicAssignment.Partitions {
				memberPlan = append(memberPlan, topicPartition{
					topicAssignment.Topic,
					partition,
				})
			}
		}
	}

	return memberPlan, generation
}

// initAllConsumersPartitions initializes the two "2All" fields in our
// balancer.
//
// Note that the Java code puts topic partitions that no member is interested
// in into partitions2AllPotentialConsumers. This provides no benefit to any
// part of our balancing and, at worse, could change our partitions by move
// preference unnecessarily.
func (b *balancer) initAllConsumersPartitions() {
	for _, member := range b.members {
		for _, topic := range member.Topics {
			partitions, exists := b.topics[topic]
			if !exists {
				continue
			}
			for _, partition := range partitions {
				consumerPotentialPartitions := b.consumers2AllPotentialPartitions[member.ID]
				if consumerPotentialPartitions == nil {
					consumerPotentialPartitions = make(map[topicPartition]struct{})
					b.consumers2AllPotentialPartitions[member.ID] = consumerPotentialPartitions
				}

				topicPartition := topicPartition{topic, partition}
				partitionPotentialConsumers := b.partitions2AllPotentialConsumers[topicPartition]
				if partitionPotentialConsumers == nil {
					partitionPotentialConsumers = make(map[string]struct{})
					b.partitions2AllPotentialConsumers[topicPartition] = partitionPotentialConsumers
				}

				consumerPotentialPartitions[topicPartition] = struct{}{}
				partitionPotentialConsumers[member.ID] = struct{}{}
			}
		}
		// Lastly, if this is a new member, the plan everything is
		// using will not know of it. We add that it is consuming nothing
		// in that plan here.
		if _, exists := b.plan[member.ID]; !exists {
			b.plan[member.ID] = new([]topicPartition)
		}
	}

	b.setIfMemberSubscriptionsIdentical()
}

// Determines whether each member can consume the same partitions.
//
// The Java code also checks consumers2, but it also stuffs partitions that no
// members can consume into partitions2, which returns false unnecessarily.
// With our code, the maps should be reverse identical.
func (b *balancer) setIfMemberSubscriptionsIdentical() {
	var firstMembers map[string]struct{}
	var firstSet bool
	for _, members := range b.partitions2AllPotentialConsumers {
		if !firstSet {
			firstMembers = members
			firstSet = true
			continue
		}
		if !reflect.DeepEqual(members, firstMembers) {
			return
		}
	}
	b.areSubscriptionsIdentical = true
}

// assignUnassignedPartitions does what the name says.
//
// Partitions that a member was consuming but is no longer interested in, as
// well as new partitions that nobody was consuming, are unassigned.
func (b *balancer) assignUnassignedPartitions() {
	// To build a list of unassigned partitions, we visit all partitions
	// in the current plan and, if they still exist and the prior consumer
	// no longer wants to consume them, we track it as unassigned.
	// After, we add all new partitions.
	unvisitedPartitions := b.partitions2AllPotentialConsumers.cloneKeys()

	var unassignedPartitions []topicPartition
	for member, partitions := range b.plan {
		var keepIdx int
		for _, partition := range *partitions {
			// If this partition no longer exists at all, likely due to the
			// topic being deleted, we remove the partition from the member.
			if _, exists := b.partitions2AllPotentialConsumers[partition]; !exists {
				continue
			}

			delete(unvisitedPartitions, partition)

			// O(N^2), can improve TODO make members topics a map
			if !strsHas(b.members[member].Topics, partition.topic) {
				unassignedPartitions = append(unassignedPartitions, partition)
				continue
			}

			b.partitionConsumers[partition] = member
			(*partitions)[keepIdx] = partition
			keepIdx++
		}
		*partitions = (*partitions)[:keepIdx]
	}
	for unvisited := range unvisitedPartitions {
		unassignedPartitions = append(unassignedPartitions, unvisited)
	}

	// With our list of unassigned partitions, if the partition can be
	// assigned, we assign it to the least loaded member.
	for _, partition := range unassignedPartitions {
		if _, exists := b.partitions2AllPotentialConsumers[partition]; !exists {
			continue
		}
		b.assignPartition(partition)
	}
}

func (b *balancer) balance() {
	// Make two copies of our current plan: one for the balance score
	// calculation later, and one for easy steal lookup in reassigning.
	preBalancePlan := b.plan.deepClone()
	startingPlan := make(map[string]map[topicPartition]struct{}, len(preBalancePlan))
	for member, partitions := range preBalancePlan {
		memberPartitions := make(map[topicPartition]struct{}, len(*partitions))
		for _, partition := range *partitions {
			memberPartitions[partition] = struct{}{}
		}
		startingPlan[member] = memberPartitions
	}

	didReassign := b.doReassigning(startingPlan)

	if !b.isFreshAssignment && didReassign && calcBalanceScore(b.plan) >= calcBalanceScore(preBalancePlan) {
		fmt.Printf("resetting plan, score sux, before: %d, after %d\n",
			calcBalanceScore(preBalancePlan),
			calcBalanceScore(b.plan))
		b.plan = preBalancePlan
	}
}

// calcBalanceScore calculates how balanced a plan is by summing deltas of how
// many partitions each member is consuming. The lower the aggregate delta, the
// beter.
func calcBalanceScore(plan membersPartitions) int {
	absDelta := func(l, r int) int {
		v := l - r
		if v < 0 {
			return -v
		}
		return v
	}

	var score int
	memberSizes := make(map[string]int, len(plan))
	for member, partitions := range plan {
		memberSizes[member] = len(*partitions)
	}

	// Sums a triangle of size deltas.
	for member, size := range memberSizes {
		delete(memberSizes, member)
		for _, otherSize := range memberSizes {
			score += absDelta(size, otherSize)
		}
	}
	return score
}

// assignPartition looks for the first member that can assume this unassigned
// partition, in order from members with smallest partitions, and assigns
// the partition to it.
func (b *balancer) assignPartition(unassigned topicPartition) {
	b.planByNumPartitions.Ascend(func(item btree.Item) bool {
		memberWithFewestPartitions := item.(memberWithPartitions)
		member := memberWithFewestPartitions.member
		memberPotentials := b.consumers2AllPotentialPartitions[member]
		if _, memberCanUse := memberPotentials[unassigned]; !memberCanUse {
			return true
		}

		// Before we change the sort order, delete this item from our
		// btree. If we edo this after changing the order, the tree
		// will not be able to delete the item.
		b.planByNumPartitions.Delete(item)

		partitions := memberWithFewestPartitions.partitions
		*partitions = append(*partitions, unassigned)
		// Add the item back to its new sorted position.
		b.planByNumPartitions.ReplaceOrInsert(memberWithFewestPartitions)

		b.partitionConsumers[unassigned] = member
		return false
	})
}

// doReassigning loops trying to move partitions until the plan is balanced
// or until no moves happen.
func (b *balancer) doReassigning(startingPlan map[string]map[topicPartition]struct{}) (didReassign bool) {
	downstreamFromTo := make(map[string]map[string][]topicPartition) // up => down => what down wants from up
	downstreamToFrom := make(map[string]map[string]int)              // down => who it is on up, and how many we want to steal
	downstreamRegistered := make(map[string]struct{})
	modified := true
	for modified {
		modified = false
		b.planByNumPartitions.Ascend(func(item btree.Item) bool {
			leastLoaded := item.(memberWithPartitions)
			myMember := leastLoaded.member
			fmt.Println("on", myMember)
			myPartitions := *leastLoaded.partitions

			if _, isDownstreamed := downstreamRegistered[myMember]; isDownstreamed {
				fmt.Println("I am downstream, skipping")
				return true
			}

			if len(myPartitions) == len(b.consumers2AllPotentialPartitions[myMember]) {
				fmt.Println("I have all I can have!")
				return true
			}

			// We, the least loaded member, try to steal a partition we can own
			// from the most-loaded member of all members owning our partitions.
			type stealCandidate struct {
				member    string
				partition topicPartition
			}
			var stealCandidates []stealCandidate
			var mostOtherPartitions int
			for partition := range b.consumers2AllPotentialPartitions[myMember] {
				otherMember := b.partitionConsumers[partition]
				if otherMember == leastLoaded.member {
					continue
				}

				otherPartitions := *b.plan[otherMember]
				_, otherIsDownstreamed := downstreamToFrom[otherMember]

				if (len(myPartitions) < len(otherPartitions) || otherIsDownstreamed && len(myPartitions) == len(otherPartitions)) &&
					len(otherPartitions) >= mostOtherPartitions {

					if mostOtherPartitions > 0 &&
						mostOtherPartitions < len(otherPartitions) {
						fmt.Println("resetting steal candidates, found member with higher partitions", len(otherPartitions))
						stealCandidates = stealCandidates[:0]
					}
					mostOtherPartitions = len(otherPartitions)
					fmt.Printf("found candidate with %d partitions to steal from %s: %v\n", mostOtherPartitions, otherMember, partition)
					stealCandidates = append(stealCandidates, stealCandidate{
						otherMember,
						partition,
					})
				}
			}

			if len(stealCandidates) == 0 {
				// TODO save pivot to always go GTE this
				fmt.Println("no steal candidates :(")
				return true
			}

			steal := stealCandidates[0]

			// If the candidate members have only one more partition than us,
			// then we conditionally steal.
			// If we know stealing will help a dependent member, we steal and
			// bubble down our help.
			// Otherwise, _we_ register that we are a dependent member on what
			// we would steal.
			if mostOtherPartitions == len(myPartitions)+1 {
				// If there is a negative delta downstream of us, we steal!
				if downstreamTo, hasDownstream := downstreamFromTo[myMember]; hasDownstream {
					b.reassignPartition(steal.partition, steal.member, myMember)
					fmt.Printf("%s: saw downstreamTo, stealing t %s p %d from %s\n", myMember, steal.partition.topic, steal.partition.partition, steal.member)
					b.bubbleDownstream(myMember, downstreamTo, downstreamFromTo)

					didReassign = true
					modified = true
					return false
				}

				// Stealing any partition in this set will not help our score.
				// Record among all members that, if they overflow, they can
				// offload to us.
				for _, candidate := range stealCandidates {
					downstreamTo := downstreamFromTo[candidate.member]
					if downstreamTo == nil {
						downstreamTo = make(map[string][]topicPartition)
						downstreamFromTo[candidate.member] = downstreamTo
					}
					fmt.Printf("registering downstream %s from %s under %s\n", candidate.partition.topic, myMember, candidate.member)
					downstreamTo[myMember] = append(downstreamTo[myMember], candidate.partition)

					downstreamFrom := downstreamToFrom[myMember]
					if downstreamFrom == nil {
						downstreamFrom = make(map[string]int)
						downstreamToFrom[myMember] = downstreamFrom
					}
					downstreamFrom[candidate.member]++
				}
				downstreamRegistered[myMember] = struct{}{}
				return true
			}

			// If the candidate members have equal partitions to us, then
			// the candidate must be downstream of something.
			// We steal, and continue stealing up.
			if mostOtherPartitions == len(myPartitions) {
				for _, candidate := range stealCandidates {
					downstreamTo := downstreamFromTo[candidate.member]
					if downstreamTo == nil {
						downstreamTo = make(map[string][]topicPartition)
						downstreamFromTo[candidate.member] = downstreamTo
					}
					fmt.Printf("registering downstream %s from %s under %s\n", candidate.partition.topic, myMember, candidate.member)
					downstreamTo[myMember] = append(downstreamTo[myMember], candidate.partition)

					downstreamFrom := downstreamToFrom[myMember]
					if downstreamFrom == nil {
						downstreamFrom = make(map[string]int)
						downstreamToFrom[myMember] = downstreamFrom
					}
					downstreamFrom[candidate.member]++
					downstreamRegistered[myMember] = struct{}{}
				}
				b.bubbleDownUpstream(myMember, downstreamFromTo, downstreamToFrom)
				return true
			}

			fmt.Printf("%s: stealing t %s p %d from %s\n", myMember, steal.partition.topic, steal.partition.partition, steal.member)

			b.reassignPartition(steal.partition, steal.member, myMember)
			didReassign = true
			modified = true
			return false
		})

	}
	return didReassign
}

func (b *balancer) bubbleDownstream(
	fromMember string,
	downstreamTo map[string][]topicPartition,
	downstreamFromTo map[string]map[string][]topicPartition,
) {
	fmt.Printf("bubbling downstream from %s\n", fromMember)
	for downstreamTo != nil {
		var downMember string
		var downPotentials []topicPartition
		for downMember, downPotentials = range downstreamTo {
			break
		}
		steal := downPotentials[len(downPotentials)-1]
		delete(downstreamTo, downMember)
		fmt.Printf("chose %s from %s to %s to bubble downstream\n", steal.topic, fromMember, downMember)
		b.reassignPartition(steal, fromMember, downMember)
		downstreamTo = downstreamFromTo[downMember]
		fromMember = downMember
	}
}

type downstreams struct {
	// stealWantersByWhoCanServe maps members to downstream members who
	// want a partition.
	// If B has 2 partitions and A has 3, B is downstream from A and
	// wants one partition.
	// The second map (B) holds the partitions from A that B wants.
	// The third map level can be a slice, but is a map for lookup
	// purposes.
	//
	// Left to right, FROM B, A wants any of X partitions.
	stealWantersByWhoCanServe map[string]map[string]map[topicPartition]struct{}

	// waitingStealersToStealees is the reverse of the above: B wants from A.
	// The second map (A) holds how many partitions B wants from A.
	// B could want more than one if there are more dependent levels:
	// say both C and D have 2, and B has 2, then there three wants
	// from A.
	waitingStealersToStealees map[string]wantSteals
}

type wantSteals struct {
	numWant     int
	whoCanServe map[string]struct{}
}

func (d *downstreams) addPartitionWant(victim, me string, partition topicPartiion) {
	stealWantersFromVictim := d.stealWantersByWhoCanServe[victim]
	if stealWantersFromVictim == nil {
		stealWantersFromVictim = make(map[string]map[topicPartition]struct{})
		d.stealWantersByWhoCanServe[victim] = stealWantersFromVictim
	}

	myWantsFromVictim := stealWantersFromVictim[me]
	if myWantsFromVictim == nil {
		myWantsFromVictim = make(map[topicPartition]struct{})
		stealWantersFromVictim[me] = myWantsFromVictim
	}

	// Register that to wants any partitions in the set from from.
	fmt.Printf("registering downstream %s from %s under %s\n", partition.topic, victim, me)
	myWantsFromVictim[partition] = struct{}{}

	myStealWants := d.waitingStealersToStealees[me]
	myStealWants.numWant++
	if myStealWants.whoCanServe == nil {
		myStealWants.whoCanServe = make(map[string]struct{})
	}
	myStealWants.whoCanServe[victim] = struct{}{}

	// We also need to add in anything waiting on us.
	for stealWantersOfMyself := range d.stealWantersByWhoCanServe[me] {
		for stealWanterOfMyself := range stealWantersOfMyself {
			myStealWants.whoCanServe.numWant += d.waitingStealersToStealees[stealWanterOfMyself].numWant
		}
	}

	d.waitingStealersToStealees[me] = myStealWants
}

// trackFromTo records a movement of a partition from from to to.
func (d *downstreams) trackStolenPartition(victim, me string, partition topicPartition) {
	myWantsFromVictim := d.stealWantersByWhoCanServe[victim][me]
	delete(myWantsFromVictim, partition)
	// If there is no more possibility to steal from from to to, we delete
	// stop tracking to under from.
	var stopWantingFromVictim bool
	if len(myWantsFromVictim) == 0 {
		stopWantingFromVictim = true
		delete(d.stealWantersByWhoCanServe[victim], me)
		// If nobody wants to steal from from anymore, we delete from.
		if len(d.stealWantersByWhoCanServe[victim]) == 0 {
			delete(d.stealWantersByWhoCanServe, victim)
		}
	}

	myStealWants := d.waitingStealersToStealees[me]
	myStealWants.numWant--
	if stopWantingFromVictim {
		delete(myStealWants, victim)
	}
	if myStealWants.numWant == 0 {
		delete(d.waitingStealersToStealees, me)
	} else {
		d.waitingStealersToStealees[me] = myStealWants
	}
}

func (b *balancer) bubbleDownUpstream(
	toMember string,
	d *downstreams,
) {
	fmt.Println("PLAN BEFORE BUBBLIN DOWN UP")
	for member, partitions := range b.plan {
		fmt.Printf("%s => %v\n", member, *partitions)
	}
	fmt.Printf("bubbling down upstream to %s\n", toMember)
	on := toMember
	for {
		// Who can we take from?
		takeFroms := downstreamToFrom[on]
		if takeFroms == nil {
			// If nobody, then the prior loop reached the top.
			break
		}
		var takeFrom string
		for takeFrom = range takeFroms {
			break
		}
		takeFroms[takeFrom]--
		if takeFroms[takeFrom] == 0 {
			delete(takeFroms, takeFrom)
		}

		var downPotentials []topicPartition
		for _, downPotentials = range downstreamFromTo[takeFrom] {
			break
		}

		steal := downPotentials[len(downPotentials)-1]
		fmt.Printf("stealing %s from upstream %s to %s\n", steal.topic, takeFrom, on)
		b.reassignPartition(steal, takeFrom, on)
		on = takeFrom
	}

	fmt.Println("done bubbling up down, current plan")
	for member, partitions := range b.plan {
		fmt.Printf("%s => %v\n", member, *partitions)
	}
	fmt.Println("maybe bubbling downstream")

	if downstreamTo, hasDownstream := downstreamFromTo[toMember]; hasDownstream {
		b.bubbleDownstream(toMember, downstreamTo, downstreamFromTo)
	}
}

// reassignPartition reassigns a partition from srcMember to dstMember, potentially
// undoing a prior move if this detects a partition when there-and-back.
// 2*O(log members)
func (b *balancer) reassignPartition(partition topicPartition, srcMember, dstMember string) {
	oldPartitions := b.plan[srcMember]
	newPartitions := b.plan[dstMember]

	// Remove the elements from our btree before we change the sort order.
	b.planByNumPartitions.Delete(memberWithPartitions{
		srcMember,
		oldPartitions,
	})
	b.planByNumPartitions.Delete(memberWithPartitions{
		dstMember,
		newPartitions,
	})

	for idx, oldPartition := range *oldPartitions { // remove from old member
		if oldPartition == partition {
			(*oldPartitions)[idx] = (*oldPartitions)[len(*oldPartitions)-1]
			*oldPartitions = (*oldPartitions)[:len(*oldPartitions)-1]
			break
		}
	}
	*newPartitions = append(*newPartitions, partition) // add to new

	fmt.Println("reassign results")
	fmt.Printf("%s => %v\n", srcMember, *oldPartitions)
	fmt.Printf("%s => %v\n", dstMember, *newPartitions)

	// Now add back the changed elements to our btree.
	b.planByNumPartitions.ReplaceOrInsert(memberWithPartitions{
		srcMember,
		oldPartitions,
	})
	b.planByNumPartitions.ReplaceOrInsert(memberWithPartitions{
		dstMember,
		newPartitions,
	})

	// Finally, update which member is consuming the partition.
	b.partitionConsumers[partition] = dstMember
}