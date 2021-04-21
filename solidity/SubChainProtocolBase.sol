pragma solidity ^0.4.11;

/**
 * @title SubChainProtocolBase.sol
 * @author David Chen
 * @dev 
 * Subchain definition for application.
 * SCS need to use this contract to register/withdraw
 * from the subchain.
 * Requires : none
 * Required by: SubChainBase.sol
 */

contract SysContract {
    function delayedSend(uint256 _blk, address _to, uint256 _value, bool bonded) public returns (bool success);
}


contract SubChainProtocolBase {
    enum SCSStatus { notRegistered, performing, withdrawPending, initialPending, withdrawDone, badActor }

    struct SCS {
        address from; //address as id
        uint256 bond;   // value
        uint256 state; // one of SCSStatus
        uint256 registerBlock;
        uint256 withdrawBlock;
    }

    struct SCSApproval {
        uint256 bondApproved;
        uint256 bondedCount;
        address[] subchainAddr;
        uint256[] amount;
    }

    mapping(address => SCS) public scsList;
    mapping(address => SCSApproval) public scsApprovalList;

    uint256 public scsCount;
    string public subChainProtocol;
    uint256 public bondMin;
    uint256 public constant PENDING_BLOCK_DELAY = 5; // 8 minutes
    uint256 public constant WITHDRAW_BLOCK_DELAY = 8640; // one day, given 10s block rate
    SysContract internal constant SYS_CONTRACT = SysContract(0x0000000000000000000000000000000000000065);

    //monitor if subchain is inactive
    //this is used to allow node to exit from zoombie subchain
    mapping(address => uint256) public subChainLastActiveBlock;
    mapping(address => uint256) public subChainExpireBlock;

    //events
    event Registered(address scs);
    event UnRegistered(address sender);

    address[] public scsArray;
    uint256 public protocolType;
    uint256 public poolSize = 50;
    address public owner;

    //constructor
    function SubChainProtocolBase(string protocol, uint256 bmin, uint256 _protocolType) public {
        scsCount = 0;
        subChainProtocol = protocol;
        bondMin = bmin;
        protocolType = _protocolType;
        owner = msg.sender;
    }

    function() public payable {  // todo: david review
        revert();
    }
    
    function updatePoolSize(uint256 _size) public {
        require(owner == msg.sender);
        poolSize = _size;
    }

    // register for SCS
    // SCS will be notified through 3rd party communication method. SCS will need to register here manually.
    // One protocol base can have several different subchains.
    function register(address scs) public payable returns (bool) {
        //already registered or not enough bond
        require(scsArray.length < poolSize);
        require(
            (scsList[scs].state == uint256(SCSStatus.notRegistered)
            || scsList[scs].state == uint256(SCSStatus.performing))
            && msg.value >= bondMin * 10 ** 18
        );

        addScsId(scs);

        scsList[scs].from = scs;
        if (scsList[scs].state == uint256(SCSStatus.notRegistered)) {
            //if not register before, update
            scsList[scs].registerBlock = block.number + PENDING_BLOCK_DELAY;
            scsList[scs].withdrawBlock = 2 ** 256 - 1;
            scsCount++;
            scsList[scs].bond = msg.value;
        } else {
            //add more fund
            scsList[scs].bond += msg.value;            
        }
        scsList[scs].state = uint256(SCSStatus.performing);
        return true;
    }

    // withdrawRequest for SCS
    function withdrawRequest() public returns (bool success) {
        //only can withdraw when active
        require(scsList[msg.sender].state == uint256(SCSStatus.performing));

        //need to make sure node is not working for any suchain anymore
        require(scsApprovalList[msg.sender].bondedCount == 0 );        

        scsList[msg.sender].withdrawBlock = block.number;
        scsList[msg.sender].state = uint256(SCSStatus.withdrawPending);
        scsCount--;

        removeScsId(msg.sender);

        UnRegistered(msg.sender);
        return true;
    }

    function withdraw() public {
        if (
            scsList[msg.sender].state == uint256(SCSStatus.withdrawPending)
            && block.number > (scsList[msg.sender].withdrawBlock + WITHDRAW_BLOCK_DELAY)
        ) {
            scsList[msg.sender].state = uint256(SCSStatus.withdrawDone);
            scsList[msg.sender].from.transfer(scsList[msg.sender].bond);
        }
    }

    function isPerforming(address _addr) public view returns (bool res) {
        return (scsList[_addr].state == uint256(SCSStatus.performing) && scsList[_addr].registerBlock < block.number);
    }

    function getSelectionTarget(uint256 thousandth, uint256 minnum) public view returns (uint256 target) {
        // find a target to choose thousandth/1000 of total scs
        if (minnum < 50) {
            minnum = 50;
        }

        if (scsCount < minnum) {          // or use scsCount* thousandth / 1000 + 1 < minnum
            return 255;
        }

        uint256 m = thousandth * scsCount / 1000;

        if (m < minnum) {
            m = minnum;
        }

        target = (m * 256 / scsCount + 1) / 2;

        return target;
    }

    function getSelectionTargetByCount(uint256 targetnum) public view returns (uint256 target) {
        if (targetnum == 0 ) {
            return 0;
        } else if (scsCount <= targetnum) {        
            return 255;
        }

        //calculate distance
        target = (targetnum * 256 / scsCount + 1) / 2;

        return target;
    }


    //display approved scs list
    function approvalAddresses(address addr) public view returns (address[]) {
        address[] memory res = new address[](scsApprovalList[addr].bondedCount);
        for (uint256 i = 0; i < scsApprovalList[addr].bondedCount; i++) {
            res[i] = (scsApprovalList[addr].subchainAddr[i]);
        }
        return res;
    }

    //display approved amount array
    function approvalAmounts(address addr) public view returns (uint256[]) {
        uint256[] memory res = new uint256[](scsApprovalList[addr].bondedCount);
        for (uint256 i = 0; i < scsApprovalList[addr].bondedCount; i++) {
            res[i] = (scsApprovalList[addr].amount[i]);
        }
        return res;
    }

    //subchain need to set this before allow nodes to join
    function setSubchainExpireBlock(uint256 blk) public {
        subChainExpireBlock[msg.sender] = blk;
    }

    //set active block
    function setSubchainActiveBlock() public {
        subChainLastActiveBlock[msg.sender] = block.number;
    }

    //approve the bond to be deduced if act maliciously
    function approveBond(address scs, uint256 amount, uint8 v, bytes32 r, bytes32 s) public returns (bool) {
        //require subchain is active
        //require( (subChainLastActiveBlock[msg.sender] + subChainExpireBlock[msg.sender])  > block.number);

        //make sure SCS is performing
        if (!isPerforming(scs)) {
            return false;
        }

        //verify signature
        //combine scs and subchain address
        bytes32 hash = sha256(scs, msg.sender);

        //verify signature matches.
        if (ecrecover(hash, v, r, s) != scs) {
            return false;
        }

        //check if bond still available for SCSApproval
        if (scsList[scs].bond < (scsApprovalList[scs].bondApproved + amount)) {
            return false;
        }

        //add subchain info
        scsApprovalList[scs].bondApproved += amount;
        scsApprovalList[scs].subchainAddr.push(msg.sender);
        scsApprovalList[scs].amount.push(amount);
        scsApprovalList[scs].bondedCount++;

        return true;
    }

    //must called from SubChainBase
    function forfeitBond(address scs, uint256 amount) public payable returns (bool) {
        //require( (subChainLastActiveBlock[msg.sender] + subChainExpireBlock[msg.sender])  > block.number);
        
        //check if subchain is approved
        for (uint256 i = 0; i < scsApprovalList[scs].bondedCount; i++) {
            if (scsApprovalList[scs].subchainAddr[i] == msg.sender && scsApprovalList[scs].amount[i] == amount) {
                //delete array item by moving the last item in current postion and delete the last one
                scsApprovalList[scs].bondApproved -= amount;
                scsApprovalList[scs].bondedCount--;
                scsApprovalList[scs].subchainAddr[i]
                    = scsApprovalList[scs].subchainAddr[scsApprovalList[scs].bondedCount];
                scsApprovalList[scs].amount[i] = scsApprovalList[scs].amount[scsApprovalList[scs].bondedCount];

                delete scsApprovalList[scs].subchainAddr[scsApprovalList[scs].bondedCount];
                delete scsApprovalList[scs].amount[scsApprovalList[scs].bondedCount];
                scsApprovalList[scs].subchainAddr.length--;
                scsApprovalList[scs].amount.length--;

                //doing the deduction
                scsList[scs].bond -= amount;
                //scsList[scs].state = uint256(SCSStatus.badActor);
                msg.sender.transfer(amount);

                return true;
            }
        }

        return false;
    }


    //scs to request to release from a subchain if subchain is not active
    //anyone can request this
    function releaseRequest(address scs, address subchain) public returns (bool) {
        //check subchain info
        for (uint256 i=0; i < scsApprovalList[scs].bondedCount; i++) {
            if (scsApprovalList[scs].subchainAddr[i] == subchain && 
                (subChainLastActiveBlock[subchain] + subChainExpireBlock[subchain])  < block.number) {
                scsApprovalList[scs].bondApproved -= scsApprovalList[scs].amount[i];
                scsApprovalList[scs].bondedCount--;
                scsApprovalList[scs].subchainAddr[i]
                    = scsApprovalList[scs].subchainAddr[scsApprovalList[scs].bondedCount];
                scsApprovalList[scs].amount[i] = scsApprovalList[scs].amount[scsApprovalList[scs].bondedCount];

                //clear
                delete scsApprovalList[scs].subchainAddr[scsApprovalList[scs].bondedCount];
                delete scsApprovalList[scs].amount[scsApprovalList[scs].bondedCount];
                scsApprovalList[scs].subchainAddr.length--;
                scsApprovalList[scs].amount.length--;

                //DAVID: not send back bond. It only happens in withdraw request. 
                //just make node out of subchain
                return true;
            }
        }
        return false;
    }

    //subchain to request to release a scs from a subchain
    function releaseFromSubchain(address scs, uint256 amount) public returns (bool) {
        //check subchain info
        for (uint256 i=0; i < scsApprovalList[scs].bondedCount; i++) {
            if (scsApprovalList[scs].subchainAddr[i] == msg.sender && scsApprovalList[scs].amount[i] == amount) {
                scsApprovalList[scs].bondApproved -= amount;
                scsApprovalList[scs].bondedCount--;
                scsApprovalList[scs].subchainAddr[i]
                    = scsApprovalList[scs].subchainAddr[scsApprovalList[scs].bondedCount];
                scsApprovalList[scs].amount[i] = scsApprovalList[scs].amount[scsApprovalList[scs].bondedCount];

                //clear
                delete scsApprovalList[scs].subchainAddr[scsApprovalList[scs].bondedCount];
                delete scsApprovalList[scs].amount[scsApprovalList[scs].bondedCount];
                scsApprovalList[scs].subchainAddr.length--;
                scsApprovalList[scs].amount.length--;

                //DAVID: not send back bond. It only happens in withdraw request. 
                //just make node out of subchain
                return true;
            }
        }
        return false;
    }

    function addScsId(address scsId) private {
        if (scsList[scsId].state == uint256(SCSStatus.notRegistered)) {
            scsArray.push(scsId);
        }
    }

    function removeScsId(address scsId) private {
        uint256 len = scsArray.length;
        for (uint256 i=len; i>0; i--) {
            if (scsArray[i-1] ==  scsId) {
                scsArray[i-1] = scsArray[len - 1];
                delete scsArray[len - 1];
                scsArray.length--;
            }
        }
    }
}
