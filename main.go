package main

import (
	"context"
	"fmt"
	v1 "k8s.io/api/core/v1"
	"log"
	"strings"

	"github.com/volcengine/volcengine-go-sdk/service/natgateway"
	"github.com/volcengine/volcengine-go-sdk/volcengine"
	"github.com/volcengine/volcengine-go-sdk/volcengine/credentials"
	"github.com/volcengine/volcengine-go-sdk/volcengine/session"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const (
	AK = "AK"
	SK = "SK"
)

func main() {
	// 目标：获取pod上面的注解
	config, err := rest.InClusterConfig()
	if err != nil {
		panic(err.Error())
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		panic(err.Error())
	}
	// labels.NewRequirement("adjust-snat-controller.alphagodzilla/", op selection.Operator, vals []string, opts ...field.PathOption)
	// fields.EscapeValue(s string)
	pods, err := clientset.CoreV1().Pods("").List(context.TODO(), metav1.ListOptions{
		FieldSelector: fmt.Sprintf("metadata.annotations[%s]=[%s]", "adjust-snat-controller.alphagodzilla/enable", "true"),
	})
	if len(pods.Items) <= 0 {
		return
	}
	natGatewayIdMap := make(map[string][]*v1.Pod, 0)
	for _, pod := range pods.Items {
		ann := pod.Annotations
		_, ok := ann["adjust-snat-controller.alphagodzilla/enable"]
		if !ok {
			continue
		}
		ngi, ok := ann["adjust-snat-controller.alphagodzilla/nat-gateway-id"]
		if !ok {
			continue
		}
		_, ok = ann["adjust-snat-controller.alphagodzilla/eip-id"]
		if !ok {
			continue
		}
		entry, ok := natGatewayIdMap[ngi]
		if !ok {
			natGatewayIdMap[ngi] = make([]*v1.Pod, 0)
			natGatewayIdMap[ngi] = append(natGatewayIdMap[ngi], &pod)
		} else {
			entry = append(entry, &pod)
		}
	}
	// 创建VE的客户端
	veClient := create_client("", "", "")
	for ngi, tpods := range natGatewayIdMap {
		snatMap := list_nat_snat_config(veClient, ngi)
		if len(*snatMap) <= 0 {
			continue
		}
		for _, pod := range tpods {
			podIp := pod.Status.PodIP
			// expect pod bind eip
			ann := pod.Annotations
			expectEipId, _ := ann["adjust-snat-controller.alphagodzilla/eip-id"]

			sourceCidr, ok := (*snatMap)[expectEipId]
			if !ok {
				// 没有相对应的SNAT规则，就创建新的规则
				subNetId := ann["adjust-snat-controller.alphagodzilla/sub-net-id"]
				snatName := ann["adjust-snat-controller.alphagodzilla/name"]
				create_snat(veClient, ngi, expectEipId, subNetId, snatName, fmt.Sprintf("%v/32", podIp))
				continue
			}
			if !strings.HasPrefix(*sourceCidr, podIp) {
				// 不一致，说明Pod发生漂移. 修改SNAT规则
				log.Printf("Pod发生漂移IP变化，预期IP=%v， 当前IP=%v， 动作：修改SNAT规则\n", *sourceCidr, podIp)
				// 删除旧的SNAT规则
				delete_snat(veClient)
				// 创建新的SNAT规则
			}
		}
	}
}

func create_client(ak string, sk string, region string) *natgateway.NATGATEWAY {
	config := volcengine.NewConfig().
		WithRegion(region).
		WithCredentials(credentials.NewStaticCredentials(ak, sk, ""))
	sess, err := session.NewSession(config)
	if err != nil {
		panic(err)
	}
	return natgateway.New(sess)
}

func list_nat_snat_config(client *natgateway.NATGATEWAY, natGatewayId string) *map[string]*string {
	describeNatGatewaysInput := &natgateway.DescribeNatGatewaysInput{
		NatGatewayIds: volcengine.StringSlice([]string{natGatewayId}),
	}
	resp, err := client.DescribeNatGateways(describeNatGatewaysInput)
	if err != nil {
		panic(err)
	}
	// fmt.Println(resp)
	snat_map := make(map[string]*string, 0)
	for _, natEntry := range resp.NatGateways {
		snatIds := make([]*string, 0)
		for _, snatEntryId := range natEntry.SnatEntryIds {
			// snatIds[0] =
			snatIds = append(snatIds, snatEntryId)
		}
		if len(snatIds) <= 0 {
			continue
		}
		describeSnatEntriesInput := &natgateway.DescribeSnatEntriesInput{
			NatGatewayId: natEntry.NatGatewayId,
			SnatEntryIds: snatIds,
		}
		snatResp, err := client.DescribeSnatEntries(describeSnatEntriesInput)
		if err != nil {
			panic(err)
		}
		for _, snatEntry := range snatResp.SnatEntries {
			sourceCidr := snatEntry.SourceCidr
			eipId := snatEntry.EipId
			//snat_entries = append(snat_entries, map[string]*string{
			//	"sourceCidr": sourceCidr,
			//	"eipAddress": eipAddress,
			//})
			snatEntry := snatEntry.SnatEntryId
			snat_map[*eipId] = sourceCidr
		}
	}
	return &snat_map
}

func delete_snat(client *natgateway.NATGATEWAY, snatId string) {
	deleteSnatEntryInput := &natgateway.DeleteSnatEntryInput{
		SnatEntryId: volcengine.String(snatId),
	}

	resp, err := client.DeleteSnatEntry(deleteSnatEntryInput)
	if err != nil {
		panic(err)
	}
	log.Printf("删除SNAT规则，%v, %v\n", snatId, resp)
}

func create_snat(client *natgateway.NATGATEWAY, NatGatewayId string, EipId string, SubnetId string, SnatEntryName string, SourceCidr string) {
	createSnatEntryInput := &natgateway.CreateSnatEntryInput{
		EipId:         volcengine.String(EipId),
		NatGatewayId:  volcengine.String(NatGatewayId),
		SnatEntryName: volcengine.String(SnatEntryName),
		SubnetId:      volcengine.String(SubnetId),
		SourceCidr:    volcengine.String(SourceCidr),
	}
	resp, err := client.CreateSnatEntry(createSnatEntryInput)
	if err != nil {
		panic(err)
	}
	log.Printf("创建SNAT规则，%v\n", resp)
}
